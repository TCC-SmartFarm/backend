#!/usr/bin/env bash
# Prepara na VM os arquivos de ambiente do irrigacao-mss. Rodar UMA vez, antes
# do push que leva o serviço irrigacao-mss no docker-compose.prod.yml — sem
# env/irrigacao-mss.env o `up -d` do deploy falha para a stack inteira.
#
# Da máquina local, sem copiar nada para a VM:
#   ssh -i ~/.ssh/id_rsa azureuser@smartfarm-tcc.chilecentral.cloudapp.azure.com 'bash -s' < scripts/setup-irrigacao-env.sh
#
# O que faz (idempotente, não reinicia nenhum container):
#   1. cria env/irrigacao-mss.env, reaproveitando SUPABASE_URL/SUPABASE_KEY do
#      env/mqtt-sub.env (a chave sb_secret_ — nunca sai do servidor);
#   2. acrescenta IRRIGATION_MSS_URL ao env/api-service.env.
# O api-service só lê a variável nova quando for recriado; o próprio deploy
# faz isso, porque o compose recria o contêiner quando o env_file muda.
set -euo pipefail

cd ~/smartfarm
umask 077

src=env/mqtt-sub.env
dst=env/irrigacao-mss.env
api=env/api-service.env

for f in "$src" "$api"; do
	[ -f "$f" ] || { echo "ERRO: $f não existe em ~/smartfarm" >&2; exit 1; }
done

if [ -f "$dst" ]; then
	echo "$dst já existe — mantido como está"
else
	supabase_url=$(grep -m1 '^SUPABASE_URL=' "$src" || true)
	supabase_key=$(grep -m1 '^SUPABASE_KEY=' "$src" || true)
	if [ -z "$supabase_url" ] || [ -z "$supabase_key" ]; then
		echo "ERRO: SUPABASE_URL/SUPABASE_KEY não encontradas em $src" >&2
		exit 1
	fi

	cat > "$dst" <<EOF
# irrigacao-mss — gerado por scripts/setup-irrigacao-env.sh em $(date -u +%FT%TZ)
PORT=8456

# Leituras recentes vêm do api-service, pela rede interna do compose.
SENSOR_API_URL_TEMPLATE=http://api-service:3000/api/sensors/latest/{user}/{devEUI}

# Localização do dispositivo (users.devices). Mesma chave do mqtt-sub.
$supabase_url
$supabase_key

IRRIGATION_TIMEZONE=America/Sao_Paulo

# Desloca as leituras para que a mais recente caia em "agora". Remendo para o
# relógio quebrado do firmware — decidir com o Murilo antes de ligar.
TEMP_FIX_SENSOR_TIMESTAMP=false
EOF
	echo "$dst criado"
fi

if grep -q '^IRRIGATION_MSS_URL=' "$api"; then
	echo "IRRIGATION_MSS_URL já está em $api — mantido"
else
	# garante quebra de linha antes de acrescentar
	[ -z "$(tail -c1 "$api")" ] || echo >> "$api"
	echo 'IRRIGATION_MSS_URL=http://irrigacao-mss:8456' >> "$api"
	echo "IRRIGATION_MSS_URL acrescentada a $api"
fi

ls -l "$dst" "$api"
