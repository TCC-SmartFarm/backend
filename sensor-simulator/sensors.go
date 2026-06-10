package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// Sensor descreve um dispositivo simulado. Os ids, nomes e posições foram
// portados do script scripts/simulate_sensors.sh do front-end: 10 sensores ao
// redor do centro da fazenda (-23.6484655, -46.5739827), ~0.001 grau ≈ 111 m.
type Sensor struct {
	ID   string
	Name string
	Lat  string
	Lon  string
}

const (
	centerLat = -23.6484655
	centerLon = -46.5739827
)

func coord(center, offset float64) string {
	return fmt.Sprintf("%.7f", center+offset)
}

var sensors = []Sensor{
	{"1e23a01", "Plantacao Norte", coord(centerLat, 0.0020), coord(centerLon, 0.0010)},
	{"1e23a02", "Plantacao Sul", coord(centerLat, -0.0015), coord(centerLon, 0.0022)},
	{"1e23a03", "Plantacao Leste", coord(centerLat, 0.0008), coord(centerLon, -0.0018)},
	{"1e23a04", "Plantacao Oeste", coord(centerLat, -0.0025), coord(centerLon, 0.0015)},
	{"1e23a05", "Estufa A", coord(centerLat, 0.0012), coord(centerLon, -0.0028)},
	{"1e23a06", "Estufa B", coord(centerLat, -0.0007), coord(centerLon, 0.0007)},
	{"1e23a07", "Pomar Velho", coord(centerLat, 0.0030), coord(centerLon, -0.0012)},
	{"1e23a08", "Horta Central", coord(centerLat, -0.0018), coord(centerLon, 0.0025)},
	{"1e23a09", "Pasto Alto", coord(centerLat, 0.0005), coord(centerLon, -0.0020)},
	{"1e23a10", "Pasto Baixo", coord(centerLat, -0.0010), coord(centerLon, 0.0003)},
}

// Reading é o payload publicado no MQTT. O formato precisa bater exatamente
// com o que mqtt-sub/influx-connector/cache-service esperam: latitude e
// longitude como STRING e timestamp em segundos unix (o Influx rejeita
// escrita se o tipo de um field mudar).
type Reading struct {
	Name            string  `json:"name"`
	SoilTemperature float64 `json:"soil_temperature"`
	SoilMoisture    float64 `json:"soil_moisture"`
	AirHumidity     float64 `json:"air_humidity"`
	Luminosity      float64 `json:"luminosity"`
	AirTemperature  float64 `json:"air_temperature"`
	Battery         float64 `json:"battery"`
	Latitude        string  `json:"latitude"`
	Longitude       string  `json:"longitude"`
	Timestamp       int64   `json:"timestamp"`
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// sensorBias dá personalidade fixa a cada sensor (ex: estufa mais úmida que
// pasto) derivada do id, para as séries não ficarem idênticas entre sensores.
func sensorBias(id string) float64 {
	var h uint32
	for _, c := range id {
		h = h*31 + uint32(c)
	}
	return float64(h%100)/100.0 - 0.5 // [-0.5, 0.5)
}

// genReading gera uma leitura plausível para o horário informado: luminosidade
// e temperatura seguem o ciclo dia/noite, solo segue o ar com amortecimento e
// a bateria decai ~20% a cada 30 dias a partir de batteryStart.
func genReading(s Sensor, ts time.Time, batteryStart time.Time, rng *rand.Rand) Reading {
	local := ts.Local()
	hourF := float64(local.Hour()) + float64(local.Minute())/60.0
	bias := sensorBias(s.ID)

	// Temperatura do ar: senoide com pico ~14h (média 22, amplitude 9)
	airT := 22 + 9*math.Sin(2*math.Pi*(hourF-8)/24) + bias*2 + (rng.Float64()-0.5)*3

	// Luminosidade: curva solar entre 6h e 18h, quase zero à noite
	var lux float64
	if hourF >= 6 && hourF < 18 {
		lux = 95000 * math.Sin(math.Pi*(hourF-6)/12) * (0.85 + rng.Float64()*0.15)
	} else {
		lux = rng.Float64() * 50
	}

	// Solo segue o ar com amortecimento; umidades inversas à temperatura
	soilT := clamp(airT*0.6+8+(rng.Float64()-0.5)*1.6, 16, 28)
	soilM := clamp(52+bias*30+10*math.Sin(2*math.Pi*float64(ts.Unix())/(86400*7))+(rng.Float64()-0.5)*6, 30, 75)
	airH := clamp(110-2*airT+bias*10+(rng.Float64()-0.5)*6, 45, 90)

	// Bateria: 100% no início, -20% a cada 30 dias, piso em 20%
	days := ts.Sub(batteryStart).Hours() / 24
	batt := clamp(100-days*(20.0/30.0)+(rng.Float64()-0.5), 20, 100)

	return Reading{
		Name:            s.Name,
		SoilTemperature: round2(soilT),
		SoilMoisture:    round2(soilM),
		AirHumidity:     round2(airH),
		Luminosity:      round2(clamp(lux, 0, 100000)),
		AirTemperature:  round2(airT),
		Battery:         round2(batt),
		Latitude:        s.Lat,
		Longitude:       s.Lon,
		Timestamp:       ts.Unix(),
	}
}
