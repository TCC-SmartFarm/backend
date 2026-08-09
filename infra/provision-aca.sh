#!/usr/bin/env bash
#
# Provisiona a camada web do SmartFarm no Azure Container Apps.
#
# Equivale, no modelo de referência da AWS, ao conjunto
# "Application Load Balancer + Auto Scaling Group + Launch Template":
# o ingress do Container Apps faz o balanceamento e as regras de escala
# fazem o papel da política de Target Tracking.
#
# A camada de ingestão (mqtt-broker, RabbitMQ, Redis) permanece na VM —
# são componentes com estado e não podem ser replicados. Ver
# docs/infraestrutura-nuvem.md, seção 4.1.
#
# Pré-requisitos:
#   - az CLI autenticado na assinatura correta
#   - extensão containerapp instalada (o script instala se faltar)
#   - variáveis de ambiente com os segredos (ver bloco "Configuração")
#
# Uso:
#   set -a; source ./env/api-service.env; set +a
#   ./infra/provision-aca.sh
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuração
# ---------------------------------------------------------------------------
RG="${RG:-rg-smartfarm}"
LOC="${LOC:-chilecentral}"
VNET="${VNET:-vm-smartfarmVNET}"
SUBNET_ACA="${SUBNET_ACA:-snet-aca}"
SUBNET_ACA_CIDR="${SUBNET_ACA_CIDR:-10.0.1.0/27}"   # /27 = mínimo do ambiente de workload profiles
ENV_NAME="${ENV_NAME:-cae-smartfarm}"
APP_NAME="${APP_NAME:-api-service}"
IMAGE="${IMAGE:-ghcr.io/tcc-smartfarm/api-service:latest}"
NSG="${NSG:-vm-smartfarmNSG}"
VM_PRIVATE_IP="${VM_PRIVATE_IP:-10.0.0.4}"
KV_NAME="${KV_NAME:-kv-smartfarm-tcc}"              # precisa ser único globalmente

# Segredos e configuração da aplicação — devem vir do ambiente, nunca deste arquivo.
: "${INFLUX_URL:?defina INFLUX_URL}"
: "${INFLUX_TOKEN:?defina INFLUX_TOKEN}"
: "${INFLUX_ORG:?defina INFLUX_ORG}"
: "${INFLUX_BUCKET:?defina INFLUX_BUCKET}"
: "${AUTH_DOMAIN:?defina AUTH_DOMAIN}"
: "${AUTH_AUDIENCE:?defina AUTH_AUDIENCE}"
: "${REDIS_PASSWORD:?defina REDIS_PASSWORD (a mesma senha do requirepass no compose)}"
: "${SUPABASE_URL:?defina SUPABASE_URL (cadastro de sensores)}"
: "${SUPABASE_KEY:?defina SUPABASE_KEY (cadastro de sensores)}"

echo "==> Assinatura em uso:"
az account show --query "{nome:name, id:id}" -o tsv

# ---------------------------------------------------------------------------
# 0. Extensão do CLI
# ---------------------------------------------------------------------------
az extension add --name containerapp --upgrade --only-show-errors >/dev/null 2>&1 || true
az provider register -n Microsoft.App --wait
az provider register -n Microsoft.OperationalInsights --wait

# ---------------------------------------------------------------------------
# 1. Sub-rede dedicada ao Container Apps
#
# A sub-rede é de uso exclusivo do ambiente e precisa ser delegada ao serviço.
# A VNet é 10.0.0.0/16 e só a 10.0.0.0/24 está ocupada pela VM, então a
# 10.0.1.0/27 está livre.
# ---------------------------------------------------------------------------
echo "==> Criando sub-rede ${SUBNET_ACA} (${SUBNET_ACA_CIDR})"
az network vnet subnet create \
  -g "$RG" --vnet-name "$VNET" -n "$SUBNET_ACA" \
  --address-prefixes "$SUBNET_ACA_CIDR" \
  --delegations Microsoft.App/environments \
  --only-show-errors -o none

SUBNET_ID=$(az network vnet subnet show -g "$RG" --vnet-name "$VNET" -n "$SUBNET_ACA" --query id -o tsv)

# ---------------------------------------------------------------------------
# 2. Ambiente do Container Apps
#
# --enable-workload-profiles: usa o ambiente que aceita sub-rede /27.
# Sem --internal-only, o ambiente é externo: recebe tráfego da internet com
# certificado TLS gerenciado (o equivalente ao listener do Load Balancer).
# ---------------------------------------------------------------------------
echo "==> Criando ambiente ${ENV_NAME} (pode levar alguns minutos)"
az containerapp env create \
  -g "$RG" -n "$ENV_NAME" -l "$LOC" \
  --infrastructure-subnet-resource-id "$SUBNET_ID" \
  --enable-workload-profiles true \
  --only-show-errors -o none

# ---------------------------------------------------------------------------
# 3. Key Vault (equivalente ao AWS Secrets Manager)
# ---------------------------------------------------------------------------
echo "==> Criando Key Vault ${KV_NAME}"
az keyvault create \
  -g "$RG" -n "$KV_NAME" -l "$LOC" \
  --enable-rbac-authorization true \
  --only-show-errors -o none

KV_ID=$(az keyvault show -g "$RG" -n "$KV_NAME" --query id -o tsv)
ME=$(az ad signed-in-user show --query id -o tsv)

# Permissão para o próprio operador gravar os segredos
az role assignment create \
  --assignee "$ME" --role "Key Vault Secrets Officer" --scope "$KV_ID" \
  --only-show-errors -o none 2>/dev/null || true

echo "    aguardando propagação da permissão RBAC..."
sleep 30

az keyvault secret set --vault-name "$KV_NAME" -n influx-token   --value "$INFLUX_TOKEN"   -o none
az keyvault secret set --vault-name "$KV_NAME" -n redis-password --value "$REDIS_PASSWORD" -o none
az keyvault secret set --vault-name "$KV_NAME" -n supabase-key   --value "$SUPABASE_KEY"   -o none

# ---------------------------------------------------------------------------
# 4. Container App
#
# min 1 / max 5 réplicas — o equivalente ao mínimo e máximo do Auto Scaling Group.
# A regra de concorrência HTTP dispara réplicas novas quando passa de 50
# requisições simultâneas por réplica (equivalente à Target Tracking Policy).
# ---------------------------------------------------------------------------
echo "==> Criando container app ${APP_NAME}"
az containerapp create \
  -g "$RG" -n "$APP_NAME" --environment "$ENV_NAME" \
  --image "$IMAGE" \
  --target-port 3000 --ingress external \
  --cpu 0.25 --memory 0.5Gi \
  --min-replicas 1 --max-replicas 5 \
  --scale-rule-name http-concurrency \
  --scale-rule-type http \
  --scale-rule-http-concurrency 50 \
  --env-vars \
      INFLUX_URL="$INFLUX_URL" \
      INFLUX_ORG="$INFLUX_ORG" \
      INFLUX_BUCKET="$INFLUX_BUCKET" \
      AUTH_DOMAIN="$AUTH_DOMAIN" \
      AUTH_AUDIENCE="$AUTH_AUDIENCE" \
      REDIS_ADDR="${VM_PRIVATE_IP}:6379" \
      SUPABASE_URL="$SUPABASE_URL" \
  --only-show-errors -o none

# Identidade gerenciada: é ela que autoriza o app a ler o Key Vault,
# sem nenhuma credencial armazenada na aplicação.
echo "==> Habilitando identidade gerenciada e concedendo acesso ao Key Vault"
APP_PRINCIPAL=$(az containerapp identity assign \
  -g "$RG" -n "$APP_NAME" --system-assigned --query principalId -o tsv)

az role assignment create \
  --assignee "$APP_PRINCIPAL" --role "Key Vault Secrets User" --scope "$KV_ID" \
  --only-show-errors -o none

sleep 30

# Segredos referenciando o Key Vault (não copiados para o app)
KV_URI=$(az keyvault show -g "$RG" -n "$KV_NAME" --query properties.vaultUri -o tsv)
az containerapp secret set -g "$RG" -n "$APP_NAME" \
  --secrets \
     influx-token="keyvaultref:${KV_URI}secrets/influx-token,identityref:system" \
     redis-password="keyvaultref:${KV_URI}secrets/redis-password,identityref:system" \
     supabase-key="keyvaultref:${KV_URI}secrets/supabase-key,identityref:system" \
  --only-show-errors -o none

az containerapp update -g "$RG" -n "$APP_NAME" \
  --set-env-vars \
     INFLUX_TOKEN=secretref:influx-token \
     REDIS_PASSWORD=secretref:redis-password \
     SUPABASE_KEY=secretref:supabase-key \
  --only-show-errors -o none

# ---------------------------------------------------------------------------
# 5. Sondas de saúde
#
# O endpoint /health já existe no api-service e não exige autenticação.
# A sonda de readiness é o que permite ao ingress tirar do balanceamento uma
# réplica que subiu mas ainda não está pronta.
# ---------------------------------------------------------------------------
echo "==> Configurando probes em /health"
TMP_YAML=$(mktemp)
az containerapp show -g "$RG" -n "$APP_NAME" -o yaml > "$TMP_YAML"
python - "$TMP_YAML" <<'PY'
import sys, yaml
path = sys.argv[1]
with open(path) as f:
    doc = yaml.safe_load(f)
probes = [
    {"type": "Liveness",  "httpGet": {"path": "/health", "port": 3000},
     "initialDelaySeconds": 10, "periodSeconds": 30},
    {"type": "Readiness", "httpGet": {"path": "/health", "port": 3000},
     "initialDelaySeconds": 5,  "periodSeconds": 10},
]
for c in doc["properties"]["template"]["containers"]:
    c["probes"] = probes
with open(path, "w") as f:
    yaml.safe_dump(doc, f, sort_keys=False)
PY
az containerapp update -g "$RG" -n "$APP_NAME" --yaml "$TMP_YAML" --only-show-errors -o none
rm -f "$TMP_YAML"

# ---------------------------------------------------------------------------
# 6. Segurança de rede (equivalente aos Security Groups em camadas)
#
# O Redis passa a aceitar conexões APENAS da sub-rede do Container Apps.
# Nenhum outro endereço, dentro ou fora da VNet, alcança a porta 6379.
# ---------------------------------------------------------------------------
echo "==> Regra de NSG: 6379 somente da sub-rede ${SUBNET_ACA_CIDR}"
az network nsg rule create \
  -g "$RG" --nsg-name "$NSG" -n allow-redis-from-aca \
  --priority 200 --direction Inbound --access Allow --protocol Tcp \
  --source-address-prefixes "$SUBNET_ACA_CIDR" \
  --destination-port-ranges 6379 \
  --only-show-errors -o none

# ---------------------------------------------------------------------------
# 7. Resultado
# ---------------------------------------------------------------------------
FQDN=$(az containerapp show -g "$RG" -n "$APP_NAME" --query properties.configuration.ingress.fqdn -o tsv)
echo
echo "======================================================================"
echo " Camada web provisionada."
echo
echo " URL da API : https://${FQDN}"
echo " Health     : https://${FQDN}/health"
echo " Réplicas   : 1 a 5 (gatilho: 50 requisições simultâneas por réplica)"
echo
echo " Próximos passos:"
echo "   1. Publicar o Redis no IP privado com senha (docker-compose.prod.yml)"
echo "   2. Apontar VITE_API_BASE_URL do front para a URL acima"
echo "   3. Remover caddy e api-service do compose da VM"
echo "======================================================================"
