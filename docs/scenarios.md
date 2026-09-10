# Chaos scenariji

Realne situacije koje sistem mora da preživi, svedene na ponovljive korake.
Svi su izvedeni protiv docker stack-a (`make stack-up`) sa lokalnim agentsim
fleet-om. Chaos ide kroz agente (`conductor chaos ...`), nikad direktno u bazu —
agent laže ili ćuti preko pravog gRPC transporta.

## Pragovi (internal/engine)

| Konstanta | Vrednost | Značenje |
|---|---|---|
| `sensorSweepInterval` | 5s | koliko često sensor proverava staleness |
| `hostNotReadyAfter` | 30s | tišina → host van scheduling-a (reverzibilno) |
| `hostDeadAfter` | 2min | tišina → replike se oslobađaju (jednosmerno) |
| `volumeLeaseTTL` | 90s | bez healthy observacije → lease ističe, failover sme |
| startup grace | = `hostDeadAfter` | posle boot-a engine-a nema presuda smrti dok ne protekne pun prozor |

## Postavka

```bash
make stack-up      # postgres + engine + agentsim + chaos-ui (localhost:3000)
make build
# u praznom folderu:
./build/conductor init -n chaos-demo
./build/conductor add --service --name web --image nginx:alpine
./build/conductor up -s web                    # config.toml: 2 replike, us-east-1
```

Agentsim je deo stack-a (jedan sim-agent po hostu, control API na :7780).
Chaos ide ili kroz UI (Chaos tab) ili kroz CLI; ID-jeve daje
`./build/conductor chaos agents` (host + replika + faza + chaos mod).

## 1. Mrežni blip (< 2min) — ništa se ne pomera

Zahtev: kratka smetnja ne sme da scrambluje workload.

- `conductor chaos kill-host <host>` → host ćuti
- ~30s: host `notready`, van scheduling-a; **replike ostaju vezane i active**
- `conductor chaos recover-host <host>` pre 2min
- prvi heartbeat vraća `ready`; nijedna replika nije mrdnula

Izmereno: kill 11:54:42 → notready 11:55:14 (32s) → recover 11:55:27 → ready
11:55:32 (5s). Replike netaknute ceo period.

## 2. Smrt hosta (> 2min) — re-place na preživele

Zahtev: mrtav host gubi replike; one se automatski re-place-uju na druge
hostove istog regiona; host koji kasnije oživi vraća se prazan u pool.

- `conductor chaos kill-host <host>`, ne oporavljaj
- ~30s: `notready` (kao gore)
- ~2min: sensor `MarkHostDown` — replike hostless `replacing`, sledeći tick
  placer ih dodeli drugom hostu, agent ih podigne kroz start → health → active
- `recover-host` bilo kad posle: host se vraća `ready`, prazan; orphan
  kontejnere agent sam ugasi na prvom full snapshotu (nisu više u njegovoj listi)

Izmereno: kill 11:55:47 → notready 11:56:19 (32s) → replika oslobođena
11:57:49 (2min02s) → active+healthy na novom hostu 11:57:53 (4s posle presude).

## 3. Restart control plane-a — startup grace, bez masakra

Zahtev: pad/redeploy engine-a duži od staleness prozora ne sme po boot-u da
proglasi ceo fleet mrtvim (heartbeat-i su stali zato što je gateway bio down,
ne zato što su hostovi mrtvi).

- `docker compose stop engine`, sačekaj 2-3min (svi heartbeat-i staju)
- `docker compose start engine`
- agenti se rekonektuju (gRPC backoff ume da doda i ~30s posle dužeg pada);
  sweep sme da demote-uje u `notready` (reverzibilno), ali presuda smrti ne
  pada dok uptime engine-a ne pređe `hostDeadAfter` — do tada su se svi živi javili
- očekivano: nula oslobođenih replika, fleet se vrati `ready` bez ijednog restarta

Izmereno: outage 11:58:29→12:00:59 (2.5min) → na +30s sweep demote-ovao svih 6
hostova (agenti još u backoff-u) → agenti nazad +39s → heartbeat vratio `ready`.
`hostless=0`, `active=2` tokom celog ciklusa — nijedna replika ni restartovana
ni pomerena.

## 4. Crash-loop replike — restart budžet

Zahtev: kontejner koji stalno umire ne sme da vrti sistem u krug zauvek.

- `conductor chaos crashloop <replica>` → restart_count raste svaki tick
- reconciler-ovo crashLooping pravilo obara repliku u `failed` (terminalno)
  kad pređe `restart_max_retries`; pravi se zamenska replika
- `conductor chaos heal <replica>` nema efekta na `failed` — terminalna faza

## 5. Zaglavljen health check — progress deadline

Zahtev: deploy koji nikad ne postane healthy ne sme da visi večno.

- `conductor chaos stall <replica>` → kontejner se podigne, probe nikad ne prođu
- replika stoji u `health_check`; progress-deadline putanja je obara i
  rollout se završava kao failed umesto da visi

## 6. Zombi agent + stateful lease — single writer

Zahtev: particionisan-ali-živ agent ne sme da drži volume zauvek niti da
vaskrsne otpisanu repliku.

- stateful servis (`conductor add --database ...` + volume), replika drži lease
- `kill-host` njenog hosta; posle 2min replika ide u `replacing`
- agent nastavi da šalje healthy za nju (zombi): observacije padaju na SQL
  guard (`replacing` je orchestrator-owned) i — bitno — **ne obnavljaju lease**
- lease istekne 90s od poslednje observacije koja je stvarno prošla; tek tad
  zamenska replika sme `AcquireVolumeLease` → single writer očuvan, failover
  nije blokiran

## Regularni scenariji (bez chaosa)

### 7. Scale up / down

- `conductor scale us-east-1=4 -s web` → placer dodaje replike uz anti-affinity
  (širi po hostovima pre nego što duplira)
- `conductor scale us-east-1=2 -s web` → višak ide u `draining`, reap posle
  `drain_seconds`; traffic pointer se NE dira (scale-down nije rollout)

Izmereno: 2→4 active+healthy za **8s** (spread na 3 hosta); 4→2: draining na
+9s, reaped na +12s (drain window 10s).

### 8. Novi deploy — blue/green

- izmeni spec/image pa `conductor up -s web` → v2 replike se dižu paralelno sa
  v1; kad su sve healthy, traffic switch je atomski batch (SetServedRevision +
  drain starih u istoj transakciji); v1 replike se drain-uju pa reap-uju
- pad v2 (crash/stall pre nego što postane healthy) → progress deadline obara
  rollout kao failed, v1 ostaje da služi

Izmereno: `up` v2 → v2 current (2 active) i v1 reaped za **12s**.

### 9. Stateful servis — volume, lease, recreate

- `conductor add --database --engine postgres --name pg` +
  `conductor volume add --mount /var/lib/postgresql/data --size 2 -s pg` +
  `conductor up -s pg -f pg-config.toml` (num_replicas=1 — stateful je single
  instance)
- placer prvo smesti volume (3D bin-pack: cpu/mem/disk), replika prati volume
  host; lease se uzima u istoj transakciji kao dodela hosta
- redeploy (`up` opet) je recreate, ne blue/green: stara replika ode, nova
  preuzima lease — nikad dva pisca

Izmereno: deploy→active+lease za **13s** (replika i volume na istom hostu);
recreate v1→v2 sa lease handover-om za **19s**, ceo period tačno 1 živa replika.

### 10. Operator akcije

- `cordon` (UI ili SQL): host ostaje da služi postojeće, ne dobija novo
- `drain`: engine evakuiše replike sa hosta
- `conductor rollback`: vrati prethodnu verziju deploymenta

## Kroz chaos-ui (localhost:3000)

Chaos tab pokriva sve akcije, po targetu:

- **Replika**: Crash (terminal `failed`), Crash loop (restart_count raste dok
  ne pukne budžet), Stall health checks (progress-deadline putanja), Heal
  (skini chaos), Delete row (orphan simulacija — jedini direktan DB upis)
- **Deployment**: Crash deployment / Stall rollout — fan-out iste agent-akcije
  na sve žive replike deploymenta
- **Host**: Kill host (agent zaćuti → notready na ~30s → mrtav na 2min),
  Recover (prvi heartbeat vraća ready), Cordon i Drain (operator desired
  state, DB upis)

Sve agent-observable akcije UI prosleđuje agentsim control API-ju
(`AGENTSIM_URL`, u stacku `http://agentsim:7780`) — chaos putuje pravim
transportom (agent laže/ćuti preko gRPC-a), pa ga sledeći report ne može
pregaziti. CLI ekvivalent: `conductor chaos kill-host|recover-host|crash|
crashloop|stall|heal <id>`. Tranzicije gledaj na Topology tabu i u Logs
(`sensor -> stale hosts out of scheduling`, `sensor -> host down`,
`reconcile -> rule fired`).
