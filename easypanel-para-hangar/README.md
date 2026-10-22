# easypanel-para-hangar

Leva os projetos de um servidor com **Easypanel** para o **[Hangar](https://github.com/JuniorCarlini/hangar)**:
configuração (serviços, variáveis, discos, limites), dados (bancos e discos) e
um relatório do que precisa de mão humana (domínios, portas, Git).

É um binário só, em Go. **Não instala nada na VPS**: ele usa o `docker` do
servidor, local ou por SSH (`--host ssh://root@vps`).

## As duas formas de migrar

| | **A. Mesma VPS** (recomendada) | **B. Por fora (formatar a VPS)** |
|---|---|---|
| Como | Hangar instalado ao lado do Easypanel; importa; troca o proxy | Baixa tudo para o seu computador, formata, instala o Hangar, sobe tudo de volta |
| Fora do ar | **minutos** (só a virada do proxy e a cópia final) | **horas** (download + formatar + instalar + upload + restaurar) |
| Volta atrás | fácil: o Easypanel continua lá, parado | difícil: só pelo pacote que ficou no seu computador |
| Disco | precisa de espaço para ~2x os dados | sem limite na VPS; o pacote fica no seu computador |
| Sistema | a VPS já precisa ser Debian 13 / Ubuntu 24.04+ | aproveita a formatação para instalar o sistema certo |
| Quando usar | quase sempre | VPS com sistema antigo, disco pequeno, ou quando quer começar limpo |

Dá para fazer as duas. A **A** é a melhor porque o Easypanel continua de pé
o tempo todo, e a troca é um "desliga o Traefik, liga o Caddy". A **B** é para
quando a VPS precisa ser formatada de qualquer jeito.

### ⚠️ Antes de instalar o Hangar na mesma VPS

O instalador do Hangar, quando não existe `/etc/docker/daemon.json`, cria um
com `"live-restore": true` e reinicia o Docker. **O Docker não sobe com
live-restore em modo Swarm** — e o Easypanel roda em Swarm. Resultado: o
Docker não volta e tudo cai. Testado:

```
failed to start cluster component: --live-restore daemon configuration is incompatible with swarm mode
```

O `checar` detecta isso e mostra a correção (criar o `daemon.json` sem
live-restore antes; o instalador não mexe num arquivo que já existe).

## Comandos

```sh
# no servidor do Easypanel (ou de fora: --host ssh://root@IP)
easypanel-para-hangar checar        # dá para instalar o Hangar aqui? (forma A)
easypanel-para-hangar inventario    # o que existe: projetos, serviços, discos, domínios
easypanel-para-hangar exportar --saida PASTA [--api URL --token T] [--imagens] [--parar-apps] [--sem-dados]

# no servidor do Hangar (ou de fora: --host ssh://root@IP-novo)
easypanel-para-hangar preparar  --pacote PASTA   # carrega as imagens (forma B)
easypanel-para-hangar restaurar --pacote PASTA [--projeto loja] [--destino loja=loja-nova] [--limpar]
```

- `--api http://127.0.0.1:3000 --token ...` (token em Configurações › Usuários
  do Easypanel): opcional. Sem ele tudo funciona, só não fica anotado de qual
  repositório Git cada app vinha.
- `--parar-apps`: para as apps no Easypanel antes de copiar os discos (cópia
  consistente). O comando para religar fica em `parados.txt`.
- `--imagens`: grava no pacote as imagens que o Easypanel construiu (forma B).

## Passo a passo — A. Mesma VPS

1. `easypanel-para-hangar checar` e resolver o que for **CRÍTICO**.
2. Instalar o Hangar (README dele). Com o Traefik do Easypanel nas portas
   80/443, o Hangar instala sem o proxy dele e o painel fica pelo túnel SSH
   (`ssh -L 7700:127.0.0.1:7700 root@vps` → `http://localhost:7700`).
3. **Ensaio** (sem parar nada): `exportar --saida /root/ensaio --api ... --token ...`,
   importar e `restaurar`. Conferir as apps pelo endereço `sslip.io` do
   Hangar ou pelo túnel. Apagar os projetos de ensaio no Hangar.
4. **Virada** (fora do horário de pico):
   1. `exportar --saida /root/migracao --parar-apps --api ... --token ...`
   2. No Hangar, para cada projeto: criar o projeto com o mesmo nome →
      **Novo serviço › Compose** → colar `projetos/<p>/compose.yaml` →
      **Analisar** → **Criar**.
   3. `restaurar --pacote /root/migracao`
   4. Parar o Traefik do Easypanel (`docker service scale easypanel-traefik=0`;
      confira o nome em `docker service ls`),
      ligar `[edge]` e `[panel] public` no `/etc/hangar/hangar.toml` e
      `systemctl restart hangard`.
   5. Cadastrar os domínios (lista no `MIGRACAO.md` de cada projeto). O DNS
      não muda: é o mesmo IP.
5. Deixar o Easypanel parado uns dias como plano de volta. Voltar = parar o
   proxy do Hangar, `docker service scale easypanel-traefik=1` e os comandos
   do `parados.txt`.

## Passo a passo — B. Por fora

1. No seu computador (com Docker CLI e acesso SSH por chave à VPS):
   `easypanel-para-hangar exportar --host ssh://root@IP --saida ./migracao --imagens --parar-apps --api http://IP:3000 --token ...`
2. Conferir o pacote (`LEIA-ME.md`, os `MIGRACAO.md`, o tamanho dos dumps).
   **Guarde uma segunda cópia** antes de formatar.
3. Formatar a VPS com Debian 13 ou Ubuntu 24.04+ e instalar o Hangar.
4. `easypanel-para-hangar preparar --host ssh://root@IP --pacote ./migracao`
5. Importar os `compose.yaml` no Hangar (como no passo 4.2 da forma A).
6. `easypanel-para-hangar restaurar --host ssh://root@IP --pacote ./migracao`
7. Domínios, portas e pendências pelo `MIGRACAO.md`.

## O que vira o quê

| Easypanel | Hangar |
|---|---|
| PostgreSQL | template PostgreSQL (versão do Hangar, senha nova) + `pg_restore` de cada banco |
| Redis | template Redis (cache, ou filas se tinha `--appendonly yes`) + cópia de `/data` |
| MySQL / MariaDB / MongoDB | serviço por imagem, mesma versão e variáveis + dump/restore |
| App por imagem | serviço por imagem, mesmas variáveis, discos, comando, memória/CPU |
| App construída pelo Easypanel (Git, Dockerfile, Nixpacks) | a **mesma imagem já construída**, como `ep2hangar.invalid/<p>-<s>:migracao`; depois troque para Git no Hangar |
| Volume / Bind Mount | disco do Hangar com o conteúdo copiado |
| `DATABASE_URL=postgres://...@loja_db:5432/...` | `${{db.DATABASE_URL}}` (o Hangar religa sozinho) |
| Domínios | lista no `MIGRACAO.md` (cadastrar na aba Domínios) |
| Portas publicadas | lista no `MIGRACAO.md` (o Hangar não abre porta sozinho) |
| File Mount | conteúdo salvo em `dados/.../arquivos/`; o Hangar não monta arquivos |
| Serviço Compose | arquivos copiados para `projetos/<p>/compose/<s>/` (importar à mão) |

O registro `ep2hangar.invalid` não existe de propósito: o Hangar tenta baixar
a imagem, falha, e usa a que está no servidor. Um nome sem registro iria para
o Docker Hub, onde outra pessoa poderia publicar uma imagem com esse nome.

## Testado

Num Docker em Swarm imitando o Easypanel (`loja`: Postgres 16 com dois
bancos, Redis com AOF e senha, app construída localmente com bind mount, File
Mount, porta publicada e dois domínios no Traefik; `blog`: MariaDB e um
serviço parado):

- `inventario`, `checar` e `exportar` (com `--imagens --parar-apps`);
- o `compose.yaml` gerado passou pelo analisador de compose do próprio Hangar
  (`projects.ParseCompose`) sem nenhum bloqueio: Postgres e Redis viraram
  templates e `DATABASE_URL`/`REDIS_URL` viraram `${{db.DATABASE_URL}}` e
  `${{redis.REDIS_URL}}`;
- `preparar` + `restaurar` em containers e discos com os nomes que o Hangar
  usa: dados conferidos no Postgres 18, Redis, disco de uploads e MariaDB.

Ainda **não** testado num Easypanel de verdade: o formato da API (varia entre
versões; ela é opcional) e o caminho `/etc/easypanel/traefik/config` dos
domínios. Rode `inventario` primeiro e confira.

## Limites conhecidos

- Até 20 serviços por `compose.yaml` e 8 discos por serviço (limites do Hangar).
- Bancos extras do PostgreSQL voltam no mesmo container, mas o Hangar só
  "conhece" o principal (backups e referências).
- O MongoDB e o MySQL viram serviço por imagem: sem backups automáticos do
  Hangar.
- Projetos que se falavam pela rede do Easypanel (`blog_mysql` dentro do
  `loja`) precisam ser conectados no Hangar (aba Rede); o relatório aponta.

## Desenvolvimento

```sh
go test ./...
go build -o easypanel-para-hangar .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o easypanel-para-hangar-linux-amd64 .
```
