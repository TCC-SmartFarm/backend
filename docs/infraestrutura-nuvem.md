# Infraestrutura em Nuvem do SmartFarm — arquitetura, alternativas e custos

> Documento de apoio ao TCC. Compara duas arquiteturas de nuvem para a plataforma SmartFarm:
> uma **completa**, que espelha fielmente o modelo de referência de alta disponibilidade, e uma
> **enxuta**, efetivamente adotada. Explica o papel de cada componente, o custo de cada opção e
> o critério de decisão.
>
> Preços coletados em agosto de 2026 na região **Brazil South**, via API de preços de varejo da
> Azure. Câmbio de referência: **US$ 1,00 = R$ 5,10**.
>
> **Revisões:** atualizado após a integração da branch `develop`, que trocou o broker MQTT próprio
> por um network server LoRaWAN externo e introduziu o Supabase como cadastro de dispositivos. Em
> agosto de 2026 ganhou a seção 6, que documenta a execução da migração — o que efetivamente mudou
> de lugar, por que apenas um serviço pôde sair da VM e o que substituiu o proxy reverso próprio.

---

## 1. Resumo executivo

O SmartFarm rodava, até agosto de 2026, com **todos os componentes de servidor numa única máquina
virtual** — o que, no modelo de referência adotado na disciplina de infraestrutura, corresponde à
*Fase 2* do projeto: uma prova de conceito sem balanceamento de carga e sem alta disponibilidade.

Este documento avalia o salto para as *Fases 3 e 4* daquele modelo (desacoplar o banco de dados e
colocar a camada web atrás de um balanceador com escalabilidade automática) e conclui:

| | Camada web | Total da solução |
|---|---|---|
| **Arquitetura completa** (Load Balancer + VM Scale Set) | US$ 144,08/mês (R$ 735) | US$ 225,04/mês (R$ 1.148) |
| **Arquitetura enxuta** (Azure Container Apps) | US$ 4,29/mês (R$ 22) | US$ 52,21/mês (R$ 266) |
| **Economia** | **97%** | **77%** |

A arquitetura enxuta foi a escolhida. Ela entrega **as mesmas capacidades funcionais** exigidas pelo
modelo — balanceamento de carga entre réplicas, escalabilidade automática com faixa mínima e máxima,
segurança em camadas e segredos gerenciados — porque a plataforma Azure Container Apps já embute
esses mecanismos. O que se abre mão é da *materialização* deles como recursos separados e
configuráveis individualmente.

> **Estado atual:** a arquitetura B foi **implementada em agosto de 2026**. O `api-service` roda no
> Azure Container Apps e a VM ficou apenas com a camada de ingestão. A seção 6 documenta como a
> migração foi feita, por que apenas um serviço mudou de lugar e o que se ganhou ao remover o proxy
> reverso próprio.

---

## 2. Ponto de partida: o SmartFarm antes da migração

> O desenho desta seção descreve o estado **anterior** a agosto de 2026, e é o ponto de partida da
> análise. O `caddy` e o `api-service` que aparecem dentro da VM saíram dela na migração — ver 6.1 e
> 6.5.

```
Sensores LoRa ──▶ Gateway ──▶ networkserver2.maua.br  (ChirpStack, externo à nossa infra)
                                        │
                                        │ MQTT — conexão de SAÍDA, iniciada pela VM
                                        ▼
        ┌──────────────────── VM única (2 vCPU / 4 GB) ────────────────────┐
        │  mqtt-sub ──▶ RabbitMQ ──┬──▶ influx-connector ──▶ InfluxDB Cloud │
        │      │                   └──▶ cache-service ─────▶ Redis         │
        │      └──▶ Supabase  ("de quem é este devEUI?")                   │
        │                                                                  │
        │  Caddy (TLS) ──▶ api-service ──▶ Redis + InfluxDB + Supabase     │
        └──────────────────────────────────────────────────────────────────┘
                                        ▲
                            Navegador ──┘ HTTPS

Navegador ──HTTPS──▶ Azure Static Web Apps (front React)
```

Características relevantes para esta análise:

- **Um único ponto de falha.** Se a VM cair, a ingestão de telemetria e a API caem juntas.
- **Sem balanceamento.** Existe uma só instância da API; não há como distribuir carga.
- **Sem escalabilidade.** Um pico de acessos degrada a aplicação inteira.
- **A ingestão não recebe conexões.** O `mqtt-sub` é um *cliente* que assina um tópico no network
  server da Mauá. Isso é uma mudança importante em relação ao desenho anterior, em que um broker
  MQTT próprio precisava aceitar conexões dos sensores — hoje **a VM não precisa de nenhuma porta
  de entrada além do SSH** (detalhado em 2.1).
- **O cadastro de dispositivos vive fora da nossa infra**, no Supabase (Postgres gerenciado como
  serviço). Ele é a fonte de verdade de qual sensor pertence a qual produtor.
- **Componentes com e sem estado convivem** na mesma VM — condição que determina o que pode ou não
  ser replicado (seção 4.1).

### 2.1 Consequência arquitetural da mudança para o network server

Vale destacar porque muda a superfície de ataque e o desenho de rede:

| | Desenho anterior (broker próprio) | Desenho atual (network server externo) |
|---|---|---|
| Origem dos dados | Sensores conectavam **na nossa VM** | Gateway entrega ao network server da Mauá |
| Sentido da conexão | **Entrada** (VM aceitava) | **Saída** (VM conecta) |
| Portas expostas | 1883 / 8883 abertas ao mundo | Nenhuma |
| Autenticação de dispositivo | ACL própria no broker (ClientID = deviceId) | Responsabilidade do network server |
| Identificação do dono | Vinha no tópico (`userId/{fazenda}/...`) | Consulta ao Supabase pelo `devEUI` |

O broker próprio (`mqtt-broker`) continua no repositório e no `docker-compose`, mas **está fora do
caminho de produção** — foi mantido para desenvolvimento e testes locais.

**Contrapartida:** o network server passou a ser uma dependência externa fora do nosso controle. Se
ele ficar indisponível, a ingestão para, e não há o que fazer do nosso lado. É um risco a registrar,
não um defeito do desenho — a alternativa (operar um network server LoRaWAN próprio) tem custo e
complexidade muito maiores.

---

## 3. Modelo de referência e mapeamento para a Azure

O modelo de referência é o padrão de aplicação web altamente disponível da AWS: uma VPC com
sub-redes públicas e privadas distribuídas em duas zonas de disponibilidade, um Application Load
Balancer recebendo o tráfego externo, um Auto Scaling Group de instâncias EC2 na camada privada,
um banco relacional gerenciado (RDS) isolado da internet, segredos no Secrets Manager e
monitoramento no CloudWatch.

A tradução para a Azure é praticamente termo a termo:

| Modelo de referência (AWS) | Equivalente na Azure | Entra na versão enxuta? |
|---|---|---|
| VPC | Virtual Network (VNet) | Sim |
| Sub-redes públicas / privadas em 2 AZs | Sub-redes + Availability Zones | Parcial |
| Internet Gateway | IP público + roteamento padrão | Sim (ingress gerenciado) |
| NAT Gateway | Azure NAT Gateway | Não |
| **Application Load Balancer** | **Standard Load Balancer** ou Application Gateway | **Sim, embutido no ingress** |
| **Auto Scaling Group + Launch Template** | **Virtual Machine Scale Set** | **Sim, como réplicas** |
| Target Tracking Policy | Regras de autoscale (KEDA) | Sim |
| Alarmes do CloudWatch | Alertas do Azure Monitor | Sim |
| **RDS MySQL** | Azure Database for PostgreSQL Flexible Server | **Não — adotado o Supabase** |
| Secrets Manager | Azure Key Vault | Sim |
| Security Groups | Network Security Groups (NSG) | Sim |
| VPC Endpoint | Private Endpoint / Service Endpoint | Não |
| CloudWatch | Azure Monitor | Sim |

> **Sobre o banco relacional:** o modelo de referência pede um banco gerenciado dentro da rede
> privada. A equipe optou pelo **Supabase**, um Postgres gerenciado como SaaS, porque ele já estava
> em uso na ingestão para resolver a que produtor pertence cada `devEUI`. Manter um segundo banco só
> para a camada web duplicaria a mesma informação em dois lugares — com risco de divergência
> silenciosa entre eles. O custo dessa escolha é que o banco fica **fora da VNet**, acessado pela
> internet com TLS e chave de API, e não por sub-rede privada. É a principal divergência consciente
> em relação ao modelo (retomada em 8).

---

## 4. Arquitetura A — Completa

Esta é a arquitetura que reproduz o modelo de referência com fidelidade.

```
                    Internet
                       │
                       ▼
            ┌──────────────────────┐
            │ IP público (Standard)│
            └──────────┬───────────┘
                       ▼
         ┌─────────────────────────────┐   ◀── NSG: aceita 443 de 0.0.0.0/0
         │  Standard Load Balancer     │
         └──────┬───────────────┬──────┘
                │               │
   ── Zona 1 ───┼────────  ── Zona 2 ──┼──────────
                ▼                      ▼
        ┌───────────────┐      ┌───────────────┐   ◀── NSG: aceita 443 SÓ do LB
        │ api-service   │      │ api-service   │
        │ (sub-rede     │      │ (sub-rede     │
        │  privada 1)   │      │  privada 2)   │
        └───────┬───────┘      └───────┬───────┘
                │   VM Scale Set: 2 a 5 réplicas
                └───────────┬───────────┘
                            ▼
        ┌────────────────────────────────────┐   ◀── NSG: aceita 5432 SÓ do Scale Set
        │ PostgreSQL Flexible (sub-rede priv)│
        └────────────────────────────────────┘

        ┌──────────────┐   ┌──────────────┐   ┌──────────────┐
        │ NAT Gateway  │   │  Key Vault   │   │Azure Monitor │
        │ (saída)      │   │ (segredos)   │   │(métricas)    │
        └──────────────┘   └──────────────┘   └──────────────┘

        ┌────────────────────────────────────────────────────┐
        │ VM de ingestão: mqtt-sub + RabbitMQ + Redis        │  ◀── permanece única (ver 4.1)
        └────────────────────────────────────────────────────┘
```

### 4.1 Por que a camada de ingestão fica de fora do Scale Set

Este é o ponto técnico mais importante do documento, e vale explicitá-lo porque contraria a
intuição de "colocar tudo atrás do balanceador".

Um balanceador de carga só funciona quando as instâncias por trás dele são **intercambiáveis**:
qualquer uma pode atender qualquer requisição, porque nenhuma guarda informação que as outras não
tenham. Isso vale para o `api-service`, que é *stateless* — ele lê do Redis, do InfluxDB e do
Supabase e devolve JSON, sem guardar nada localmente. Duas cópias dele são indistinguíveis.

Não vale para o pipeline de ingestão, por dois motivos diferentes:

**a) Consumidor único.** O `mqtt-sub` mantém **uma assinatura** no tópico do network server. Duas
réplicas assinando o mesmo tópico receberiam **cada leitura duas vezes** e a gravariam duas vezes no
InfluxDB. Não é um problema de estado guardado, e sim de cardinalidade do consumo: o serviço precisa
ser singleton. (Existe saída — *shared subscriptions* do MQTT 5, em que o broker distribui as
mensagens entre os assinantes de um grupo — mas depende de suporte do network server e não foi
necessário no volume atual.)

**b) Estado local.** O **RabbitMQ** é a fila: duas instâncias independentes não formam uma fila,
formam duas filas distintas, e cada consumidor veria apenas parte das mensagens. O **Redis** guarda
o buffer das últimas leituras em memória; réplicas independentes teriam caches divergentes, e a
resposta da API variaria conforme a réplica sorteada.

Ou seja: replicar esses componentes não multiplica a capacidade — **corrompe os dados**. Escalá-los
exige clusterização (RabbitMQ em quorum queues, Redis em modo cluster) ou particionamento do consumo,
que é um projeto próprio e desnecessário no volume atual. Por isso, tanto na arquitetura completa
quanto na enxuta, a ingestão permanece numa VM única e **apenas o `api-service` é replicado**.

### 4.2 Papel e justificativa de cada componente

| Componente | Por que existe | O que acontece sem ele |
|---|---|---|
| **Virtual Network** | Isola os recursos num espaço de endereçamento privado e permite controlar o tráfego entre eles | Todo recurso precisaria de IP público, exposto à internet |
| **Sub-redes públicas / privadas** | Só o balanceador fica alcançável de fora; aplicação e banco ficam sem rota de entrada da internet | As instâncias ficariam acessíveis diretamente, sem camada intermediária |
| **Duas zonas de disponibilidade** | Uma zona é um conjunto de datacenters com energia e rede independentes; distribuir entre duas garante que uma falha física não derrube a aplicação | Uma falha de zona derruba 100% da aplicação |
| **Standard Load Balancer** | Distribui as requisições entre as réplicas e retira automaticamente as que falham no health check | Não há como usar mais de uma instância; nenhuma tolerância a falha |
| **VM Scale Set** | Cria e destrói VMs idênticas conforme a métrica de carga, dentro de uma faixa mínimo–máximo | Capacidade fixa: ou paga por capacidade ociosa, ou cai no pico |
| **Regras de autoscale** | Traduzem "CPU acima de X por Y minutos" em adicionar/remover instância | O Scale Set existiria mas nunca reagiria à demanda |
| **NAT Gateway** | Dá saída para a internet às instâncias privadas sem lhes dar IP público | As instâncias privadas ficariam sem acesso de saída, ou precisariam de IP público |
| **Banco relacional gerenciado** | Guarda o cadastro de produtores e dispositivos, com backup automático e isolamento | Sem cadastro, não há como saber de quem é cada sensor nem listá-los |
| **Key Vault** | Guarda senhas e chaves criptografadas; a aplicação as lê em tempo de execução via identidade gerenciada | Segredos em variáveis de ambiente e arquivos, em texto puro |
| **NSGs em camadas** | Cada camada só aceita conexão de quem precisa: LB da internet, aplicação do LB, cache da aplicação | Qualquer recurso comprometido daria acesso lateral aos outros |
| **Azure Monitor** | Métricas, logs e alertas; é o gatilho do autoscale e a evidência de que ele funcionou | Sem visibilidade e sem escalonamento automático |

### 4.3 Custos — Arquitetura A

| Item | Especificação | US$/mês | R$/mês |
|---|---|---|---|
| VM Scale Set (mínimo) | 2 × B2als_v2 @ US$ 0,0605/h | 88,33 | 450,48 |
| Standard Load Balancer | US$ 0,025/h (até 5 regras) + dados | 18,75 | 95,63 |
| NAT Gateway | US$ 0,045/h + dados | 33,35 | 170,09 |
| IP público (balanceador) | Standard estático, US$ 0,005/h | 3,65 | 18,62 |
| **Subtotal — camada web** | | **144,08** | **734,81** |
| VM de ingestão | 1 × B2als_v2 | 44,17 | 225,27 |
| IP público (ingestão) | Standard estático | 3,65 | 18,62 |
| **Subtotal — ingestão** | | **47,82** | **243,88** |
| PostgreSQL Flexible | B1ms @ US$ 0,035/h + 32 GB @ US$ 0,2185/GB | 32,54 | 165,95 |
| Key Vault | US$ 0,03 / 10 mil operações | 0,10 | 0,51 |
| Azure Monitor | Alertas do autoscale | 0,50 | 2,55 |
| **Subtotal — dados e segurança** | | **33,14** | **169,01** |
| **TOTAL (2 instâncias)** | | **225,04** | **1.147,70** |
| **TOTAL (5 instâncias, em pico)** | +3 × B2als_v2 | **357,55** | **1.823,51** |

> Observação sobre a região: PostgreSQL B1ms custa US$ 0,035/h em Brazil South, contra ≈ US$ 0,017/h
> nas regiões dos Estados Unidos — praticamente o dobro. A escolha por Brazil South se justifica pela
> latência para os usuários brasileiros, mas tem custo real.

---

## 5. Arquitetura B — Enxuta (adotada)

```
                    Internet
                       │  HTTPS (certificado gerenciado)
                       ▼
   ┌──────────────────────────────────────────────────┐
   │  Azure Container Apps — ingress (proxy Envoy)    │  ◀── termina TLS e distribui entre réplicas
   │  autoscale: 1 a 5 réplicas                       │      (o balanceador de carga)
   │  gatilhos: concorrência HTTP e CPU               │
   └────────────────────┬─────────────────────────────┘
                        ▼
        ┌──────────────────────────────┐
        │ api-service                  │   sub-rede /27 delegada, dentro da VNet
        │ 0,25 vCPU / 0,5 GiB          │
        └───┬──────────┬───────────┬───┘
            │          │           │
            ▼          ▼           ▼
        Redis      InfluxDB     Supabase
      (VM, IP      Cloud        (cadastro de
       privado)    (SaaS)        dispositivos)
            ▲
            │  NSG: porta 6379 aceita SÓ o CIDR da sub-rede do Container Apps
   ┌────────┴──────────────────────────────────────────┐
   │ VM de ingestão: mqtt-sub + RabbitMQ + Redis       │
   │ (sem porta de entrada — só saída para o           │
   │  network server e para o Supabase)                │
   └───────────────────────────────────────────────────┘

   Key Vault ◀── identidade gerenciada (token do InfluxDB, senha do Redis, chave do Supabase)
   Front React ──▶ Azure Static Web Apps (plano gratuito)
```

### 5.1 O que substitui o quê

A diferença essencial: na arquitetura A, balanceamento e escalonamento são **recursos que você cria
e configura**. Na arquitetura B, são **comportamentos embutidos na plataforma**.

| Arquitetura A | Arquitetura B | Observação |
|---|---|---|
| Standard Load Balancer | Ingress do Container Apps (proxy Envoy) | Termina TLS e distribui entre réplicas; certificado emitido e renovado automaticamente |
| VM Scale Set + Launch Template | Réplicas do Container App | Faixa mínimo–máximo configurável, como no Scale Set |
| Regras de autoscale + alarmes | Regras de escala (KEDA) por concorrência HTTP e CPU | Mesma semântica de "métrica acima do alvo, adiciona réplica" |
| NAT Gateway | Saída gerenciada do ambiente | O ambiente já provê conectividade de saída |
| IP público + certificado | Domínio e certificado gerenciados | Incluídos, sem custo separado |
| PostgreSQL Flexible Server | **Supabase** (Postgres gerenciado como SaaS) | Já em uso na ingestão; evita cadastro duplicado |
| Key Vault | Key Vault | Mantido — sem equivalente embutido |
| NSGs | NSGs | Mantidos — a sub-rede delegada participa da VNet normalmente |

### 5.2 O cadastro de dispositivos

O Supabase guarda, na tabela `users`, a associação entre cada produtor e seus dispositivos. Ele é
consultado nos dois sentidos:

- **Na ingestão** (`mqtt-sub`): "de quem é este `devEUI`?" — necessário porque o pacote que chega do
  network server identifica o dispositivo, não o dono.
- **Na leitura** (`api-service`): "quais dispositivos são deste produtor?" — usado pela rota
  `GET /api/sensors/devices` e pela montagem da lista em `GET /api/sensors/all`.

O segundo sentido trouxe um ganho colateral relevante de desempenho. Antes, para listar os sensores
de um usuário, a API varria o keyspace inteiro do Redis com o comando `KEYS` — uma operação O(N)
sobre as chaves de **todas** as fazendas, que bloqueia o Redis (single-threaded) durante a varredura.
Com o cadastro, a API já sabe quais chaves buscar e as lê num único *pipeline*. Como efeito adicional,
um sensor recém-cadastrado aparece na listagem mesmo antes de ter publicado a primeira leitura.

### 5.3 Custos — Arquitetura B

O Container Apps cobra por segundo de vCPU e de memória alocados, com dois preços: **ativo** (a
réplica está processando requisições) e **ocioso** (está de pé, esperando). Há uma cota gratuita
mensal por assinatura de 180.000 vCPU-segundos, 360.000 GiB-segundos e 2 milhões de requisições.

Com 1 réplica permanente de 0,25 vCPU / 0,5 GiB, ao longo de um mês (730 h = 2.628.000 s):

- vCPU: 657.000 vCPU-s − 180.000 gratuitos = 477.000 cobrados
- Memória: 1.314.000 GiB-s − 360.000 gratuitos = 954.000 cobrados
- Requisições: muito abaixo dos 2 milhões gratuitos

| Cenário | Cálculo | US$/mês |
|---|---|---|
| Réplica majoritariamente ociosa (real) | 477.000 × 0,000003 + 954.000 × 0,000003 | **4,29** |
| Réplica 100% ativa (hipótese pessimista) | 477.000 × 0,000024 + 954.000 × 0,000003 | 14,31 |
| Mínimo de réplicas igual a zero | Escala a zero quando não há tráfego | ≈ 0,00 |

O cenário realista é o primeiro: o painel faz cerca de duas requisições por sessão, com cache de
5 a 14 minutos no cliente. A réplica passa a quase totalidade do tempo ociosa.

| Item | Especificação | US$/mês | R$/mês |
|---|---|---|---|
| Container Apps (`api-service`) | 0,25 vCPU / 0,5 GiB, 1 a 5 réplicas | 4,29 | 21,88 |
| **Subtotal — camada web** | | **4,29** | **21,88** |
| VM de ingestão | 1 × B2als_v2 | 44,17 | 225,27 |
| IP público (ingestão) | Standard estático, apenas para administração | 3,65 | 18,62 |
| **Subtotal — ingestão** | | **47,82** | **243,88** |
| Supabase | Plano gratuito | 0,00 | 0,00 |
| InfluxDB Cloud | Plano gratuito | 0,00 | 0,00 |
| Azure Static Web Apps | Plano gratuito | 0,00 | 0,00 |
| Network server LoRaWAN | Infraestrutura da Mauá | 0,00 | 0,00 |
| Key Vault | Operações | 0,10 | 0,51 |
| VNet, sub-redes, NSGs | Sem custo | 0,00 | 0,00 |
| **TOTAL** | | **52,21** | **266,27** |

---

## 6. A migração executada (agosto de 2026)

As seções anteriores comparam duas arquiteturas possíveis e justificam a escolha. Esta registra
**como a arquitetura B foi efetivamente implementada** — e, mais do que a lista de comandos, procura
responder às perguntas que naturalmente aparecem quando se olha o resultado pela primeira vez.

### 6.1 O que saiu da VM, e o que não saiu

A leitura mais comum, e equivocada, ao ver "migramos para contêineres" é imaginar que cada
microsserviço ganhou sua própria máquina. Não foi isso. **Exatamente um serviço mudou de lugar.**

| Antes | Depois |
|---|---|
| VM: mqtt-broker, RabbitMQ, mqtt-sub, influx-connector, Redis, cache-service, simulador, **api-service**, **caddy** | VM: mqtt-broker, RabbitMQ, mqtt-sub, influx-connector, Redis, cache-service, simulador |
| — | Container Apps: **api-service** |

Dos sete serviços, seis continuam exatamente onde estavam. O `caddy` foi removido sem substituto
direto na VM, pelo motivo explicado em 6.5.

Vale também desfazer uma segunda confusão: **o Azure Container Apps não é uma máquina virtual.** Não
existe servidor para acessar por SSH, aplicar patch ou dimensionar. É um serviço *serverless*: entrega-se
uma imagem e uma faixa de réplicas, e a plataforma decide onde e como executar. É justamente por isso
que ele consegue escalar a zero — algo que uma VM não faz, já que ela está ligada ou desligada, e
ligada custa o mesmo ociosa ou saturada.

### 6.2 Uma aplicação, duas execuções

Durante a migração, o `api-service` esteve rodando em dois lugares ao mesmo tempo. Isso costuma soar
como "duas APIs diferentes", mas não é o caso — e a distinção esclarece bastante.

Convém separar três conceitos que a linguagem do dia a dia mistura:

| Conceito | Quantos existem |
|---|---|
| **A imagem** — o programa empacotado, publicado no GHCR | uma |
| **O contêiner** — uma *execução* dessa imagem | duas, durante o corte |
| **O host** — onde cada execução acontece | VM e Container Apps |

Uma imagem pode ser executada quantas vezes se queira, em quantos lugares se queira. A analogia
próxima é a de um executável: existe um arquivo, e dele podem-se abrir várias janelas, em máquinas
diferentes, sem que sejam programas distintos.

As duas execuções eram **intercambiáveis** porque liam exatamente as mesmas fontes: o mesmo Redis (o
da VM, alcançado pela VNet), o mesmo InfluxDB, o mesmo Supabase, validando tokens do mesmo tenant do
Auth0. A única diferença era o endereço de entrada. Foi essa equivalência que permitiu trocar o
destino do front sem nenhuma janela de indisponibilidade — o assunto de 6.6.

### 6.3 Por que só o `api-service` pôde sair

A resposta curta está em 4.1: ele é o único componente *stateless*. Recebe a requisição, consulta
Redis, InfluxDB e Supabase, devolve JSON e não guarda nada. Terminada a requisição, não há memória do
que aconteceu. Duas cópias são indistinguíveis, e é isso que torna a replicação possível.

Os demais não podem ser replicados — mas por **dois motivos distintos**, que vale não confundir,
porque as soluções seriam diferentes:

| Serviço | Impedimento | O que exigiria para escalar |
|---|---|---|
| `redis`, `rabbitmq` | **Estado local.** A informação mora dentro deles. Duas instâncias de Redis são dois caches divergentes; duas de RabbitMQ são duas filas separadas, e cada consumidor veria apenas parte das mensagens. | Clusterização: Redis cluster, quorum queues |
| `mqtt-sub` | **Cardinalidade de consumo.** Ele não guarda estado nenhum. O problema é que mantém *uma* assinatura no tópico: duas cópias receberiam cada leitura duas vezes e a gravariam em duplicidade. | *Shared subscriptions* do MQTT 5, em que o broker reparte as mensagens entre um grupo |

A distinção importa para a defesa do desenho: o `mqtt-sub` não é um obstáculo intransponível, é uma
decisão de não pagar complexidade desnecessária no volume atual.

### 6.4 O front é um caso à parte

É tentador enquadrar o front na mesma categoria do `api-service` — "também é stateless, também
escala". Mas ele é de outra natureza, e a diferença tem consequências práticas.

O `api-service`, mesmo sem estado, **é um programa em execução**: tem processo, consome CPU e memória,
precisa de réplicas. O front não executa nada no servidor. Ele é um conjunto de arquivos estáticos —
HTML, CSS e JavaScript — que o Azure Static Web Apps apenas **entrega**, replicados por uma CDN
global. Quem executa o código é o navegador do usuário.

Disso decorre que:

- Não há "instância" do front para escalar; há cópias dos arquivos em pontos de presença.
- Não há problema de coerência: os arquivos são imutáveis dentro de um mesmo deploy.
- Não há computação a cobrar, e por isso o plano gratuito basta.

E decorre também uma armadilha operacional que custou tempo na prática. Como não existe servidor lendo
configuração em tempo de execução, o endereço da API precisa ser **gravado dentro do JavaScript no
momento do build**. No código está escrito `import.meta.env.VITE_API_BASE_URL`; no arquivo publicado,
essa expressão não existe mais — foi substituída por um literal. Trocar a variável no GitHub, portanto,
**não surte efeito algum até que uma nova compilação aconteça**. Apontar o front para o Container Apps
exigiu alterar a variável de ambiente *e* disparar um novo deploy.

### 6.5 O que era o Caddy, e por que ele saiu

O `caddy` era um **proxy reverso**: um programa posicionado à frente da aplicação, que recebe as
requisições da internet e as repassa. A nomenclatura confunde — um proxy comum fica diante do
*cliente*, que navega através dele; um proxy **reverso** fica diante do *servidor*, e quem chega de
fora conversa com ele acreditando falar com a própria aplicação.

Ele existia por uma razão concreta: **o `api-service` não fala HTTPS.** É um binário Go servindo HTTP
puro na porta 3000. E o navegador proíbe que uma página carregada por HTTPS faça requisições a HTTP —
a regra de *mixed content*. Sem TLS na API, o painel simplesmente não conseguiria buscar dados.

Implementar TLS dentro da aplicação seria possível, mas herdaria o trabalho recorrente: obter o
certificado, provar a posse do domínio e **renová-lo antes do vencimento** — os da Let's Encrypt duram
90 dias, e uma renovação esquecida derruba o serviço. O Caddy resolvia isso sozinho; sua configuração
inteira eram três linhas úteis:

```
smartfarm-tcc.chilecentral.cloudapp.azure.com {
	reverse_proxy api-service:3000
}
```

O fluxo era:

```
Navegador ──HTTPS/443──▶ Caddy ──HTTP/3000──▶ api-service
              (TLS termina aqui)    (rede interna do Docker)
```

A conexão criptografada terminava no Caddy — daí o termo **terminação TLS**. Do Caddy até a aplicação
o tráfego era HTTP puro, o que é aceitável por nunca deixar a rede interna do Docker.

**O ingress do Container Apps faz exatamente isso, embutido.** É um proxy Envoy que recebe na 443,
termina TLS com certificado gerenciado pela plataforma e encaminha para a porta declarada no contêiner.
A diferença é que a renovação deixa de ser responsabilidade do projeto.

E faz uma coisa a mais que o Caddy não fazia: **distribui entre as réplicas**. O Caddy conhecia um
destino fixo, `api-service:3000`, porque havia uma instância só. O Envoy sabe quantas réplicas existem
a cada instante e reparte a carga — é ele o balanceador de carga desta arquitetura, o item que na
tabela 5.1 substitui o Standard Load Balancer.

A remoção teve ainda um efeito de segurança que o documento afirmava antes de ser verdade. Com o Caddy
no ar, a VM mantinha as portas 80 e 443 abertas para a internet. Removido, restou apenas o SSH:

| Porta | Antes | Depois |
|---|---|---|
| 22 (SSH) | aberta | aberta |
| 80 | aberta | **fechada** |
| 443 | aberta | **fechada** |

Os volumes `caddy-data` e `caddy-config` foram preservados na VM, ainda que fora da declaração do
compose. Eles guardam os certificados emitidos, e uma eventual reversão que precisasse reemiti-los
esbarraria no limite de cinco certificados duplicados por semana da Let's Encrypt.

### 6.6 A estratégia: corte em paralelo

A migração poderia ter sido feita de uma vez — desligar a VM, subir o Container Apps, apontar o front.
Optou-se pelo contrário, e a razão é que um corte direto só revela os problemas **depois** de o
sistema já estar fora do ar.

A sequência adotada foi:

1. Provisionar a infraestrutura nova (sub-rede delegada, ambiente, Key Vault, aplicação), **sem tocar
   no que estava servindo**;
2. Validar a aplicação nova de forma independente — `/health` respondendo 200, rota autenticada
   devolvendo 401 sem token, conexão com o Redis confirmada nos logs;
3. Só então apontar o front para o endereço novo;
4. Só depois de o tráfego real estar sendo atendido, remover a instância antiga e o Caddy.

Entre os passos 1 e 4 as duas execuções coexistiram — a situação descrita em 6.2. O custo dessa
coexistência é baixo (uma réplica ociosa e alguns megabytes na VM); o benefício é que, se a aplicação
nova falhasse em qualquer ponto, bastava não avançar, sem nada a reverter.

### 6.7 Escala a zero e o custo de ficar parado

O Container Apps cobra por segundo de vCPU e memória alocados. Configurada a faixa de réplicas como
**0 a 5**, a aplicação hiberna quando não há tráfego e o custo tende a zero — o terceiro cenário da
tabela em 5.3, ali tratado como hipótese e agora efetivamente adotado.

A contrapartida é a **partida a frio**. Medida na prática após um período de ociosidade:

| Requisição | Tempo |
|---|---|
| Primeira (réplica hibernada) | **27,7 s** |
| Segunda (réplica quente) | 0,28 s |

O valor ficou acima da faixa de 5 a 20 s citada na documentação da plataforma, o que se explica pelo
que a aplicação faz no boot: conectar ao Redis e baixar o JWKS do Auth0 antes de aceitar requisições.
Como a primeira chamada acontece logo após o login, a percepção do usuário é de travamento. Para
demonstrações, convém elevar o mínimo para 1 com antecedência:

```bash
az containerapp update -g rg-smartfarm -n api-service --min-replicas 1
```

Convém, porém, dimensionar a economia. A camada web hibernada poupa cerca de US$ 4/mês; **a VM
responde por mais de 90% da conta** — cerca de US$ 44/mês, cobrados integralmente esteja ela ociosa ou
não. Para períodos longos sem uso, o que efetivamente reduz o custo é desalocá-la:

```bash
az vm deallocate -g rg-smartfarm -n vm-smartfarm
```

`deallocate` difere de `stop`: apenas ele libera o hardware e interrompe a cobrança de computação.
Permanecem cobrados o disco e o IP público, que existem independentemente do estado da máquina —
algo em torno de US$ 5/mês com tudo desligado, contra os US$ 52 em operação plena. Como o IP é
estático, o nome DNS sobrevive ao ciclo de desligar e religar.

Uma dependência a registrar: o `api-service` no Container Apps lê o Redis que roda **na VM**. Com a
máquina desalocada, a aplicação sobe normalmente mas falha nas rotas de dados. Na prática, os dois
ligam e desligam juntos.

### 6.8 O que a migração não resolveu

Por honestidade de registro, três limitações permanecem:

- **A ingestão continua sendo ponto único de falha.** Se a VM cair, param a coleta e o cache. O
  Container Apps segue no ar, porém sem dado novo a servir. Nenhuma das duas arquiteturas avaliadas
  resolvia isso (4.1).
- **O banco relacional segue fora da VNet.** O Supabase é acessado pela internet com TLS e chave de
  API — a divergência consciente discutida em 3.
- **O ambiente de produção do front está desatualizado.** A branch `prod` é anterior à integração com
  o backend e sua variável `VITE_API_BASE_URL` não está configurada. Ao promovê-la, será necessário
  apontá-la para o Container Apps, sob pena de repetir o problema descrito em 6.4.

---

## 7. Comparação e decisão

| | Arquitetura A (completa) | Arquitetura B (enxuta) |
|---|---|---|
| Camada web | US$ 144,08/mês | US$ 4,29/mês |
| Total | US$ 225,04/mês | US$ 52,21/mês |
| Total anual | US$ 2.700 (R$ 13.772) | US$ 626 (R$ 3.195) |
| Balanceamento de carga | Sim, recurso dedicado | Sim, embutido no ingress |
| Escalabilidade automática | Sim, 2 a 5 VMs | Sim, 1 a 5 réplicas |
| Tempo de resposta ao pico | 2 a 4 min (boot da VM) | 5 a 20 s (start do contêiner) |
| Redundância entre zonas | Sim, explícita | Sim, se o ambiente for zone-redundant |
| Banco em sub-rede privada | Sim | Não (Supabase é externo, acessado por TLS) |
| Controle fino da rede | Total (rotas, NAT, endpoints) | Parcial |
| Esforço de operação | Alto (imagens, patches, health checks) | Baixo (plataforma gerencia) |

**A arquitetura B foi adotada.** Os motivos:

1. **A carga real não justifica a A.** O painel não faz *polling*: são cerca de duas requisições por
   sessão de usuário, com cache de 5 a 14 minutos. Duas VMs ligadas 24 horas por dia ficariam
   ociosas quase o tempo todo — pagando US$ 144/mês para servir um tráfego que uma réplica de
   0,25 vCPU atende com folga.

2. **O custo é desproporcional ao contexto.** A diferença de US$ 173/mês (R$ 882) é dinheiro real
   numa assinatura pay-as-you-go, sem crédito acadêmico para amortecer. Ao ano, são R$ 10.577.

3. **As capacidades exigidas estão preservadas.** Balanceamento entre réplicas, faixa de
   escalabilidade mínimo–máximo, política por métrica, HTTPS, segredos gerenciados e isolamento de
   rede continuam existindo — apenas fornecidos pela plataforma em vez de montados peça a peça.

4. **O escalonamento é, na prática, melhor.** Uma VM leva de 2 a 4 minutos para inicializar e entrar
   no balanceador; uma réplica de contêiner leva de 5 a 20 segundos.

**O que se perde, e é honesto registrar:**

- Não existem os recursos "Load Balancer" e "Scale Set" como objetos independentes no portal — o
  balanceamento é observável pelas métricas de réplica, não por um recurso próprio.
- O banco relacional fica fora da VNet. O Supabase é acessado pela internet com TLS e chave de API.
- Não há NAT Gateway nem controle de rotas de saída.
- A camada de ingestão continua sendo ponto único de falha em ambas as arquiteturas — essa limitação
  não é resolvida por nenhuma das duas (ver 4.1).
- O network server LoRaWAN é uma dependência externa: se ele parar, a ingestão para.

---

## 8. Cobertura dos critérios de avaliação

| Critério | Como a arquitetura B atende | Lacuna |
|---|---|---|
| **Funcionalidade** | Aplicação acessível pela internet via HTTPS, com todas as operações do painel operacionais | — |
| **Segurança** | A VM não expõe nenhuma porta de serviço (a ingestão é conexão de saída); NSG libera a porta do Redis apenas para o CIDR da sub-rede da aplicação; SSH restrito a IP conhecido; segredos no Key Vault lidos por identidade gerenciada; API exige JWT do Auth0 e deriva o usuário da *claim*, nunca da URL | Banco relacional fora da VNet |
| **Escalabilidade** | Faixa configurada de 1 a 5 réplicas, com gatilho por concorrência HTTP e por CPU, e retorno automático ao mínimo | — |
| **Disponibilidade** | Múltiplas réplicas da API, distribuídas entre zonas se o ambiente for zone-redundant; front em CDN global | Ingestão em nó único; network server externo |
| **Custos** | Réplica dimensionada para a demanda real (0,25 vCPU), mínimo ajustado ao tráfego de base, escala a zero disponível | — |
| **Balanceamento de carga** | Ingress distribui as requisições entre todas as réplicas ativas | Sem recurso de balanceador dedicado |
| **Desempenho** | Réplicas adicionais entram em 5 a 20 s; listagem de sensores resolvida por cadastro + *pipeline* no cache, sem varredura O(N) | — |

---

## 9. Evolução: quando e para onde migrar

A arquitetura enxuta não é permanente por princípio — é a adequada para a escala atual. Há dois
caminhos possíveis de evolução, e eles vão em direções opostas: um **sobe** para a arquitetura
completa, materializando balanceador e Scale Set como recursos próprios; o outro **dissolve** a VM,
substituindo os componentes com estado por serviços gerenciados. Qual faz sentido depende do que
mudar primeiro — a carga ou o hardware de campo.

### 9.1 Quando migrar para a arquitetura A

Os gatilhos que justificam a migração:

| Gatilho | Por quê |
|---|---|
| Exigência de banco relacional dentro da rede privada | Conformidade ou política que proíba banco acessível pela internet |
| Necessidade de IP de saída fixo ou firewall de egresso | Integração com terceiros que exijam lista de IPs autorizados |
| Carga sustentada acima de ~4 réplicas contínuas | A partir daí, VMs reservadas saem mais barato que consumo por segundo |
| Volume de ingestão além do que um `mqtt-sub` único absorve | Exige particionar o consumo (shared subscriptions) e clusterizar fila e cache |

Enquanto nenhum deles ocorrer, a arquitetura B entrega o mesmo resultado funcional por 23% do custo.

### 9.2 O caminho alternativo: dissolver a VM quando o sensor real entrar

Há um segundo caminho, que o restante deste documento não avalia porque depende de um evento externo:
**a entrada em operação do sensor LoRa real.** Quando isso acontecer, a VM perde a maior parte da sua
razão de existir, e passa a ser possível eliminá-la — não subindo para a arquitetura A, mas
substituindo os componentes com estado por equivalentes gerenciados.

#### O que o simulador sustenta hoje

Convém explicitar, porque é fácil subestimar: **o `sensor-simulator` é a única fonte de dados da
plataforma.** Em agosto de 2026, uma escuta de 17 minutos no tópico do network server não recebeu
nenhuma mensagem — não há hardware transmitindo. Todo o conteúdo do painel, e os 35 dias de série
histórica no InfluxDB, são gerados por ele.

E o simulador arrasta uma segunda peça: o `mqtt-broker` existe hoje **apenas para receber as
publicações dele**. Esta seção do documento já registrava que o broker próprio estava fora do caminho
de produção no desenho alvo; na prática ele voltou ao caminho justamente por causa do simulador.

Com o sensor real, o sentido da conexão se inverte e ambos saem de cena:

```
hoje:    simulador ──▶ mqtt-broker ──▶ mqtt-sub ──▶ RabbitMQ ──▶ ...
                       (hospedado na nossa VM)

depois:  sensor LoRa ──▶ gateway ──▶ network server (ChirpStack, externo)
                                            │
                                            ▼  conexão de SAÍDA
                                        mqtt-sub ──▶ RabbitMQ ──▶ ...
```

O `mqtt-sub` deixa de depender de um broker local e passa a ser cliente de um externo. A VM libera
cerca de **257 MiB** (254,6 do broker e 2,8 do simulador), caindo de ~791 MB para ~534 MB de uso.

#### O impedimento não é de infraestrutura

Antes de qualquer mudança de hospedagem, há uma decisão de projeto pendente, descoberta ao inspecionar
o decodificador do `mqtt-sub`: **o payload do sensor real não carrega tudo o que o painel exibe.**

O formato do ChirpStack traz 15 bytes com cinco medidas. Faltam duas informações que hoje vêm do
simulador:

| Parâmetro | Simulador | Payload real (15 bytes) |
|---|---|---|
| Umidade do solo | Sim | Sim |
| Umidade do ar | Sim | Sim |
| Luminosidade | Sim | Sim |
| Temperatura do ar | Sim | Sim |
| Bateria | Sim | Sim |
| **Temperatura do solo** | Sim | **Não** |
| **Latitude / longitude** | Sim | **Não** |

A ausência das coordenadas é a mais grave: o front só plota sensores com latitude e longitude
definidas, de modo que **o mapa ficaria sem nenhum marcador**. Há três saídas possíveis, e a escolha
não é de infraestrutura:

1. O firmware passa a transmitir mais bytes;
2. As coordenadas passam a vir do cadastro no Supabase — provavelmente a melhor opção, já que a
   posição de um sensor fixo não muda a cada leitura e não precisa trafegar em cada pacote LoRa;
3. O painel deixa de exibir esses dois parâmetros.

#### Sequência sugerida

Tirar o simulador é o **último** passo, não o primeiro:

1. Confirmar que há sensor real transmitindo no network server;
2. Decidir a origem da temperatura do solo e das coordenadas (o item anterior);
3. Migrar o `mqtt-sub` para o network server — a branch `develop` dele já contém essa versão;
4. Remover o `sensor-simulator` e o `mqtt-broker`;
5. Opcionalmente, avaliar dispensar a VM por completo.

#### O que o passo 5 significaria em custo

Sem o `mqtt-broker`, desaparece o único componente que exigiria ingress TCP e volume persistente — o
obstáculo que hoje inviabiliza mover a ingestão para o Container Apps. Restariam três trabalhadores
sem estado, mais os dois componentes com estado, que virariam serviços gerenciados:

| Item | US$/mês |
|---|---|
| 3 workers (`mqtt-sub`, `influx-connector`, `cache-service`), mínimo 1 réplica cada | 12,87 |
| Azure Cache for Redis, tier B0 | 16,35 |
| Azure Service Bus, tier Standard | ~10,00 |
| **Total** | **≈ 39,22** |
| | |
| Modelo atual (VM + disco Standard SSD + IP público) | 46,14 |

A economia direta é modesta — cerca de US$ 7/mês. O ganho relevante é outro: **eliminaria o ponto
único de falha da ingestão**, a limitação que nem a arquitetura A nem a B resolvem (4.1). Serviços
gerenciados trazem redundância embutida, coisa que uma VM única não tem.

Há duas contrapartidas honestas. A primeira é que se perde a capacidade de desligar: hoje um
`az vm deallocate` derruba a conta de US$ 46 para ~US$ 8, enquanto o Azure Cache for Redis e o Service
Bus cobram por existirem, com ou sem tráfego — o piso subiria para cerca de US$ 26. A segunda é que o
Service Bus não é RabbitMQ: protocolo e biblioteca diferentes, exigindo adaptar os quatro serviços que
publicam ou consomem da exchange.

---

## 10. Premissas e fontes

**Premissas de cálculo**

- Mês de 730 horas.
- Câmbio US$ 1,00 = R$ 5,10.
- Região Brazil South, preços de varejo (pay-as-you-go), sem instâncias reservadas nem descontos.
- Tráfego de dados considerado desprezível no balanceador e no NAT, dado o volume do painel.
- Cota gratuita mensal do Container Apps aplicada uma vez por assinatura.
- Supabase, InfluxDB Cloud e Static Web Apps nos respectivos planos gratuitos; a migração para
  planos pagos altera o total e deve ser reavaliada quando os limites forem atingidos.

**Fontes de preço**

- VM B2als_v2, Container Apps, Key Vault, IP público e PostgreSQL Flexible Server: API de preços de
  varejo da Azure (`prices.azure.com`), consultada em agosto de 2026 com filtro de região.
- Standard Load Balancer (US$ 0,025/h até 5 regras; US$ 0,005/GB) e NAT Gateway (US$ 0,045/h;
  US$ 0,045/GB): página pública de preços da Azure. **Estes dois medidores não são expostos pela API
  de preços** — os valores correspondem à tabela de referência e devem ser confirmados na calculadora
  da Azure para a região final antes de qualquer decisão de compra.
- Limites do Container Apps (cota gratuita, sub-rede mínima `/27` em ambiente de perfis de carga de
  trabalho, proxy Envoy no ingress): documentação oficial da Microsoft.
