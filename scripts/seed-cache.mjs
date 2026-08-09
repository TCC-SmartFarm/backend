// Popula o Redis com leituras de sensores, do mesmo jeito que o cache-service
// faria ao consumir o barramento — mas sem depender do broker MQTT nem do
// mqtt-sub (que exige credenciais do Supabase, presentes só na VM).
//
// Serve para desenvolver o front com dados locais: nada é escrito no InfluxDB
// Cloud, que é compartilhado com a produção.
//
//   node scripts/seed-cache.mjs
//   node scripts/seed-cache.mjs --users fazenda --readings 40
//
// Chave gravada: userId:<user>:devEUI:<id>:history  (lista, índice 0 = mais recente)

import net from 'node:net'

const args = process.argv.slice(2)
const flag = (name, fallback) => {
  const i = args.indexOf(`--${name}`)
  return i === -1 ? fallback : args[i + 1]
}

const HOST = flag('host', '127.0.0.1')
const PORT = Number(flag('port', '6379'))
// A claim `https://smartfarm-api/userId` do JWT é quem define a chave que o
// api-service varre. Semeamos os dois valores em uso para não depender de
// descobrir qual está no token.
const USERS = flag('users', 'fazenda,fazenda1').split(',').filter(Boolean)
const READINGS = Number(flag('readings', '20')) // cache-service faz LTRIM 0 19
const INTERVAL_MIN = Number(flag('interval', '15'))

// --- Sensores (mesmos ids, nomes e coordenadas do sensor-simulator) ---------

const CENTER_LAT = -23.6484655
const CENTER_LON = -46.5739827
const coord = (center, offset) => (center + offset).toFixed(7)

/**
 * `scenario` força parâmetros fora dos limites padrão do front, para exercitar
 * os estados do pin no mapa. `staleHours` atrasa todas as leituras do sensor,
 * deixando-o além da janela de 45 min que o front trata como offline.
 */
const SENSORS = [
  { id: '1e23a01', name: 'Plantacao Norte', dLat: 0.002, dLon: 0.001 },
  { id: '1e23a02', name: 'Plantacao Sul', dLat: -0.0015, dLon: 0.0022, scenario: { battery: 18 } },
  { id: '1e23a03', name: 'Plantacao Leste', dLat: 0.0008, dLon: -0.0018, scenario: { battery: 9 } },
  { id: '1e23a04', name: 'Plantacao Oeste', dLat: -0.0025, dLon: 0.0015, scenario: { soil_moisture: 26 } },
  { id: '1e23a05', name: 'Estufa A', dLat: 0.0012, dLon: -0.0028, scenario: { soil_moisture: 14 } },
  { id: '1e23a06', name: 'Estufa B', dLat: -0.0007, dLon: 0.0007, scenario: { soil_temperature: 32 } },
  { id: '1e23a07', name: 'Pomar Velho', dLat: 0.003, dLon: -0.0012, scenario: { soil_temperature: 37 } },
  { id: '1e23a08', name: 'Horta Central', dLat: -0.0018, dLon: 0.0025, scenario: { air_humidity: 92 } },
  { id: '1e23a09', name: 'Pasto Alto', dLat: 0.0005, dLon: -0.002 },
  { id: '1e23a10', name: 'Pasto Baixo', dLat: -0.001, dLon: 0.0003, staleHours: 3 },
]

// --- Geração de leituras ----------------------------------------------------

const round2 = (v) => Math.round(v * 100) / 100
const clamp = (v, min, max) => Math.min(max, Math.max(min, v))

// Personalidade fixa por sensor, para as séries não ficarem idênticas.
const bias = (id) => {
  let h = 0
  for (const c of id) h = (h * 31 + c.charCodeAt(0)) >>> 0
  return (h % 100) / 100 - 0.5
}

/**
 * Leitura plausível para o horário: luz e temperatura seguem o ciclo dia/noite,
 * o solo acompanha o ar amortecido. Os limites ficam com folga em relação aos
 * thresholds padrão para que só os cenários acima disparem alerta.
 */
const genReading = (sensor, date) => {
  const hour = date.getHours() + date.getMinutes() / 60
  const b = bias(sensor.id)
  const jitter = () => Math.random() - 0.5

  const airT = 22 + 9 * Math.sin((2 * Math.PI * (hour - 8)) / 24) + b * 2 + jitter() * 3
  const lux =
    hour >= 6 && hour < 18
      ? 95000 * Math.sin((Math.PI * (hour - 6)) / 12) * (0.85 + Math.random() * 0.15)
      : Math.random() * 50

  const reading = {
    name: sensor.name,
    soil_temperature: round2(clamp(airT * 0.6 + 8 + jitter() * 1.6, 16, 28)),
    // Ciclo lento de 7 dias (como no simulador), com amplitude folgada para a
    // faixa saudável não encostar no piso do clamp nem no warnLow de 30.
    soil_moisture: round2(
      clamp(56 + b * 12 + 5 * Math.sin((2 * Math.PI * (date.getTime() / 1000)) / (86400 * 7)) + jitter() * 3, 40, 72),
    ),
    air_humidity: round2(clamp(110 - 2 * airT + b * 10 + jitter() * 6, 45, 88)),
    luminosity: round2(clamp(lux, 0, 100000)),
    air_temperature: round2(airT),
    battery: round2(clamp(88 + b * 20 + jitter(), 60, 100)),
    latitude: coord(CENTER_LAT, sensor.dLat),
    longitude: coord(CENTER_LON, sensor.dLon),
    timestamp: Math.floor(date.getTime() / 1000),
  }

  // O cenário vale para toda a série, senão o gráfico mostraria o sensor
  // saudável e só o último ponto em alerta.
  return { ...reading, ...(sensor.scenario ?? {}) }
}

const envelope = (sensor, userId, payload) => ({
  userId,
  applicationId: 'smartfarm',
  deviceType: 'sensor',
  devAddr: sensor.id,
  devEUI: sensor.id,
  name: sensor.name,
  payload,
})

// --- Cliente Redis mínimo (RESP sobre TCP, sem dependências) ----------------

const encode = (...parts) => {
  const chunks = [Buffer.from(`*${parts.length}\r\n`)]
  for (const p of parts) {
    const buf = Buffer.from(String(p), 'utf8')
    chunks.push(Buffer.from(`$${buf.length}\r\n`), buf, Buffer.from('\r\n'))
  }
  return Buffer.concat(chunks)
}

const run = (commands) =>
  new Promise((resolve, reject) => {
    const socket = net.createConnection({ host: HOST, port: PORT })
    let replies = ''
    socket.on('error', reject)
    socket.on('connect', () => {
      socket.write(Buffer.concat(commands.map((c) => encode(...c))))
    })
    socket.on('data', (d) => {
      replies += d.toString()
      // Uma resposta por comando; contamos as linhas de topo pelo CRLF final.
      if (replies.split('\r\n').length > commands.length) socket.end()
    })
    socket.on('close', () => {
      const errors = replies.split('\r\n').filter((l) => l.startsWith('-'))
      errors.length ? reject(new Error(errors.join('; '))) : resolve(replies)
    })
  })

// --- Execução ---------------------------------------------------------------

const now = Date.now()
const commands = []
const summary = []

for (const sensor of SENSORS) {
  const offsetMs = (sensor.staleHours ?? 0) * 3600 * 1000
  for (const userId of USERS) {
    const key = `userId:${userId}:devEUI:${sensor.id}:history`
    commands.push(['DEL', key])
    // Do mais antigo para o mais novo: o cache-service usa LPUSH, então o
    // último empilhado fica no índice 0 — que é o que o /api/sensors/all lê.
    for (let i = READINGS - 1; i >= 0; i--) {
      const at = new Date(now - offsetMs - i * INTERVAL_MIN * 60 * 1000)
      const payload = genReading(sensor, at)
      commands.push(['LPUSH', key, JSON.stringify(envelope(sensor, userId, payload))])
      if (i === 0 && userId === USERS[0]) {
        summary.push({
          sensor: `${sensor.id} ${sensor.name}`,
          cenario: sensor.staleHours
            ? `offline há ${sensor.staleHours}h`
            : sensor.scenario
              ? Object.entries(sensor.scenario).map(([k, v]) => `${k}=${v}`).join(', ')
              : 'saudável',
          bateria: payload.battery,
          umidade_solo: payload.soil_moisture,
          temp_solo: payload.soil_temperature,
          umidade_ar: payload.air_humidity,
        })
      }
    }
  }
}

await run(commands)

console.log(`Redis ${HOST}:${PORT} — ${SENSORS.length} sensores x ${READINGS} leituras`)
console.log(`usuários semeados: ${USERS.join(', ')}\n`)
console.table(summary)
