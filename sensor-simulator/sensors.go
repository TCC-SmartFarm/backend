package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// Sensor descreve um dispositivo simulado, ao redor do centro da fazenda
// (-23.6484655, -46.5739827); ~0.001 grau ≈ 111 m.
type Sensor struct {
	ID   string
	Name string
	Lat  string
	Lon  string
	// Scenario força parâmetros fora dos limites padrão do painel depois de a
	// leitura ser gerada. Serve para o mapa exibir os três estados do pin
	// enquanto não há hardware real cadastrado. nil = sensor saudável.
	Scenario func(*Reading)
}

const (
	centerLat = -23.6484655
	centerLon = -46.5739827
)

func coord(center, offset float64) string {
	return fmt.Sprintf("%.7f", center+offset)
}

// Conjunto de demonstração: três sensores cobrindo os estados do pin no mapa —
// dentro dos limites, atenção e alerta. Os ids são os três primeiros do
// conjunto anterior de propósito: o histórico deles já está no InfluxDB, então
// as páginas de parâmetro continuam com gráficos populados.
var sensors = []Sensor{
	{
		ID: "1e23a01", Name: "Plantacao Norte",
		Lat: coord(centerLat, 0.0020), Lon: coord(centerLon, 0.0010),
	},
	{
		ID: "1e23a02", Name: "Plantacao Sul",
		Lat: coord(centerLat, -0.0015), Lon: coord(centerLon, 0.0022),
		// 18% fica entre alertLow (15) e warnLow (20): pin amarelo, ícone de bateria.
		Scenario: func(r *Reading) { r.Battery = 18 },
	},
	{
		ID: "1e23a03", Name: "Plantacao Leste",
		Lat: coord(centerLat, 0.0008), Lon: coord(centerLon, -0.0018),
		// 14% está abaixo de alertLow (20): pin vermelho, ícone de gota.
		Scenario: func(r *Reading) { r.SoilMoisture = 14 },
	},
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
	// Piso em 38, e não em 30: o limite de atenção do painel é `valor <= 30`, então
	// encostar no piso deixaria um sensor saudável permanentemente em amarelo.
	soilM := clamp(52+bias*30+10*math.Sin(2*math.Pi*float64(ts.Unix())/(86400*7))+(rng.Float64()-0.5)*6, 38, 75)
	airH := clamp(110-2*airT+bias*10+(rng.Float64()-0.5)*6, 45, 90)

	// Bateria: 100% no início, -20% a cada 30 dias, piso em 20%
	days := ts.Sub(batteryStart).Hours() / 24
	batt := clamp(100-days*(20.0/30.0)+(rng.Float64()-0.5), 20, 100)

	reading := Reading{
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

	// Vale para toda a série, inclusive o histórico semeado: sem isso o gráfico
	// mostraria o sensor saudável e só o último ponto em alerta.
	if s.Scenario != nil {
		s.Scenario(&reading)
	}

	return reading
}
