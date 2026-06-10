// sensor-simulator publica leituras falsas dos sensores da fazenda no broker
// MQTT, no mesmo formato que um dispositivo LoRa real usaria. No primeiro boot
// pode popular o histórico (SEED_HISTORY=true) e depois publica uma leitura
// por sensor a cada PUBLISH_INTERVAL.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type config struct {
	host            string
	port            string
	user            string
	pass            string
	farm            string
	publishInterval time.Duration
	seedHistory     bool
	seedDays        int
	seedPerDay      int
	markerPath      string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadConfig() config {
	interval, err := time.ParseDuration(envOr("PUBLISH_INTERVAL", "15m"))
	if err != nil {
		log.Fatal("PUBLISH_INTERVAL inválido:", err)
	}
	seedDays, _ := strconv.Atoi(envOr("SEED_DAYS", "30"))
	seedPerDay, _ := strconv.Atoi(envOr("SEED_READINGS_PER_DAY", "4"))
	return config{
		host:            envOr("MQTT_HOST", "mqtt-broker"),
		port:            envOr("MQTT_PORT", "1883"),
		user:            envOr("MQTT_USER", "fazenda1"),
		pass:            envOr("MQTT_PASS", "pass"),
		farm:            envOr("FARM", "fazenda1"),
		publishInterval: interval,
		seedHistory:     envOr("SEED_HISTORY", "false") == "true",
		seedDays:        seedDays,
		seedPerDay:      seedPerDay,
		markerPath:      envOr("MARKER_PATH", "/data/.seeded"),
	}
}

// connect cria um cliente MQTT por sensor: a ACL do broker exige que o
// ClientID seja igual ao deviceId do tópico.
func connect(cfg config, sensorID string) mqtt.Client {
	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%s", cfg.host, cfg.port)).
		SetClientID(sensorID).
		SetUsername(cfg.user).
		SetPassword(cfg.pass).
		SetAutoReconnect(true).
		SetConnectRetry(true)

	client := mqtt.NewClient(opts)
	for attempt := 1; ; attempt++ {
		token := client.Connect()
		token.Wait()
		if token.Error() == nil {
			return client
		}
		if attempt >= 5 {
			log.Fatalf("Sensor %s: erro ao conectar no broker após %d tentativas: %v", sensorID, attempt, token.Error())
		}
		log.Printf("Sensor %s: broker indisponível (%v), tentando de novo em 5s...", sensorID, token.Error())
		time.Sleep(5 * time.Second)
	}
}

func publish(client mqtt.Client, cfg config, s Sensor, r Reading, retain bool) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	topic := fmt.Sprintf("userId/%s/sensor/%s/dados", cfg.farm, s.ID)
	token := client.Publish(topic, 1, retain, payload)
	token.Wait()
	return token.Error()
}

// loadOrInitBatteryStart lê do marker o instante em que a "bateria" estava em
// 100%, para a curva de descarga continuar de onde parou entre restarts.
func loadOrInitBatteryStart(cfg config) (time.Time, bool) {
	data, err := os.ReadFile(cfg.markerPath)
	if err == nil {
		if ts, parseErr := strconv.ParseInt(string(data), 10, 64); parseErr == nil {
			return time.Unix(ts, 0), true
		}
	}
	start := time.Now()
	if cfg.seedHistory {
		start = start.Add(-time.Duration(cfg.seedDays) * 24 * time.Hour)
	}
	return start, false
}

func saveMarker(cfg config, start time.Time) {
	if err := os.WriteFile(cfg.markerPath, []byte(strconv.FormatInt(start.Unix(), 10)), 0644); err != nil {
		log.Printf("Aviso: não foi possível gravar o marker %s: %v", cfg.markerPath, err)
	}
}

func seedHistory(cfg config, clients map[string]mqtt.Client, batteryStart time.Time, rng *rand.Rand) {
	total := 0
	interval := 24 * time.Hour / time.Duration(cfg.seedPerDay)
	readings := cfg.seedDays * cfg.seedPerDay
	start := time.Now().Add(-time.Duration(cfg.seedDays) * 24 * time.Hour)

	log.Printf("Populando histórico: %d sensores x %d leituras (%d dias)...", len(sensors), readings, cfg.seedDays)
	for _, s := range sensors {
		for r := 0; r < readings; r++ {
			ts := start.Add(time.Duration(r) * interval)
			retain := r == readings-1 // mantém só a leitura mais recente como retained
			if err := publish(clients[s.ID], cfg, s, genReading(s, ts, batteryStart, rng), retain); err != nil {
				log.Printf("Sensor %s: erro ao publicar histórico: %v", s.ID, err)
				continue
			}
			total++
			time.Sleep(20 * time.Millisecond) // não inundar broker/rabbit
		}
		log.Printf("Sensor %s (%s): histórico publicado", s.ID, s.Name)
	}
	log.Printf("Histórico concluído: %d mensagens publicadas", total)
}

func publishAll(cfg config, clients map[string]mqtt.Client, batteryStart time.Time, rng *rand.Rand) {
	now := time.Now()
	for _, s := range sensors {
		if err := publish(clients[s.ID], cfg, s, genReading(s, now, batteryStart, rng), true); err != nil {
			log.Printf("Sensor %s: erro ao publicar: %v", s.ID, err)
		}
	}
	log.Printf("Leituras publicadas para %d sensores", len(sensors))
}

func main() {
	cfg := loadConfig()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	clients := make(map[string]mqtt.Client, len(sensors))
	for _, s := range sensors {
		clients[s.ID] = connect(cfg, s.ID)
	}
	log.Printf("Conectado ao broker %s:%s como %s (%d sensores)", cfg.host, cfg.port, cfg.user, len(sensors))

	batteryStart, markerExists := loadOrInitBatteryStart(cfg)
	if cfg.seedHistory && !markerExists {
		seedHistory(cfg, clients, batteryStart, rng)
	}
	if !markerExists {
		saveMarker(cfg, batteryStart)
	}

	publishAll(cfg, clients, batteryStart, rng)

	ticker := time.NewTicker(cfg.publishInterval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			publishAll(cfg, clients, batteryStart, rng)
		case <-stop:
			log.Println("Encerrando: desconectando sensores...")
			for _, c := range clients {
				c.Disconnect(250)
			}
			return
		}
	}
}
