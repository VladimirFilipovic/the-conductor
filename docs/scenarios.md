# Chaos scenariji

Realne situacije koje sistem mora da preživi, svedene na ponovljive korake.
Svi su izvedeni protiv docker stack-a (`make stack-up`) sa lokalnim agentsim
fleet-om. Chaos ide kroz agente (chaos-ui Chaos tab ili agentsim control API na :7780), nikad direktno u bazu —
agent laže ili ćuti preko pravog gRPC transporta.

chaos-ui uopšte nema pristup bazi: topologiju čita i desired state piše preko
apiserver control plane-a (`CONTROL_PLANE_URL`, podrazumevano :7080), pa UI i
CLI prolaze kroz isti project sloj. Operator chaos (cordon/drain/delete replica)
ide na isti control plane, agent chaos na agentsim.

## Pragovi (internal/engine)

| Konstanta | Vrednost | Značenje |
|---|---|---|
| `watchdogInterval` | 5s | koliko često watchdog proverava staleness |
| `hostNotReadyAfter` | 30s | tišina → host van scheduling-a (reverzibilno) |
| `hostDeadAfter` | 2min | tišina → replike se oslobađaju (jednosmerno) |
| `volumeLeaseTTL` | 90s | bez healthy observacije → lease ističe, failover sme |
| startup grace | = `hostDeadAfter` | posle boot-a engine-a nema presuda smrti dok ne protekne pun prozor |

## Postavka

```bash
make stack-up      # postgres + engine + apiserver + agentsim + chaos-ui (localhost:3000)
make build
# u praznom folderu:
./build/conductor init -n chaos-demo
./build/conductor add --service --name web --image nginx:alpine
./build/conductor up -s web                    # config.toml: 2 replike, us-east-1
```

Agentsim je deo stack-a (jedan sim-agent po hostu, control API na :7780).
Chaos ide kroz UI (Chaos tab) ili direktno na control API; ID-jeve daje
`curl localhost:7780/agents` (host + replika + faza + chaos mod). Akcija:
`curl -XPOST localhost:7780/chaos -d '{"action":"host_kill","host":"<id>"}'`
(akcije: `host_kill|host_recover`, `replica_crash|replica_crashloop|replica_stall_health|replica_heal`,
`volume_stall_resize|volume_heal` sa `"volume":"<id>"`).

## 1. Mrežni blip (< 2min) — ništa se ne pomera

Zahtev: kratka smetnja ne sme da scrambluje workload.

- chaos `host_kill` <host> → host ćuti
- ~30s: host `notready`, van scheduling-a; **replike ostaju vezane i active**
- chaos `host_recover` <host> pre 2min
- prvi heartbeat vraća `ready`; nijedna replika nije mrdnula

Izmereno kroz chaos-ui (2026-09-11): kill → notready 30s → recover → ready 1s;
replika netaknuta.

Izmereno: kill 11:54:42 → notready 11:55:14 (32s) → recover 11:55:27 → ready
11:55:32 (5s). Replike netaknute ceo period.

## 2. Smrt hosta (> 2min) — re-place na preživele

Zahtev: mrtav host gubi replike; one se automatski re-place-uju na druge
hostove istog regiona; host koji kasnije oživi vraća se prazan u pool.

- chaos `host_kill` <host>, ne oporavljaj
- ~30s: `notready` (kao gore)
- ~2min: watchdog `MarkHostDown` — replike hostless `replacing`, sledeći tick
  placer ih dodeli drugom hostu, agent ih podigne kroz start → health → active
- `recover-host` bilo kad posle: host se vraća `ready`, prazan; orphan
  kontejnere agent sam ugasi na prvom full snapshotu (nisu više u njegovoj listi)

Izmereno kroz chaos-ui (2026-09-11): notready 34s → replacing na 2min04s → active na
drugom hostu 3s kasnije; recover vraća host `ready` prazan za 1s.

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

Izmereno (apiserver kao gateway, 2026-09-11): `docker compose stop apiserver` 153s →
watchdog demote 5 hostova na +35s, nula presuda smrti → agenti nazad ~64s posle
starta (gRPC backoff) → svi `ready` na +217s, replike netaknute.

Izmereno: outage 11:58:29→12:00:59 (2.5min) → na +30s sweep demote-ovao svih 6
hostova (agenti još u backoff-u) → agenti nazad +39s → heartbeat vratio `ready`.
`hostless=0`, `active=2` tokom celog ciklusa — nijedna replika ni restartovana
ni pomerena.

## 4. Crash-loop replike — restart budžet

Zahtev: kontejner koji stalno umire ne sme da vrti sistem u krug zauvek.

- chaos `replica_crashloop <replica>` → restart_count raste svaki tick
- kad pređe `restart_max`, reconciler-ovo `crashLooping` pravilo obara **ceo
  deployment** u `failed` i `deploymentFrozen` ga zamrzava: nema re-place-a,
  nema zamene, replika ostaje da se vrti dok operator ne uradi redeploy/rollback
- to je namerno: iscrpljen restart budžet je signal za čoveka, ne za automatiku
- chaos `replica_heal <replica>` posle toga vraća kontejner u normalan hod, ali
  deployment ostaje `failed` — jedini izlaz je `conductor up`/`rollback`

Izmereno (kroz chaos-ui): restart_max=5 → deployment `failed` za ~6s; replika
ostala `starting` sa restart_count u stotinama dok nije stigao sledeći deploy.

## 5. Zaglavljen health check — progress deadline

Zahtev: deploy koji nikad ne postane healthy ne sme da visi večno.

- chaos `replica_stall_health` <replica> → kontejner se podigne, probe nikad ne prođu
- replika stoji u `health_check`; progress-deadline putanja je obara i
  rollout se završava kao failed umesto da visi

Izmereno kroz chaos-ui (progress_deadline=60): kanarinac stall → deployment `failed`
za 56s, served revision ostao na staroj verziji. Napomena: kontejner postoji na agentu
tek ~3s posle deploya, stall pre toga vraća 404 (`no agent runs replica`).

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

Izmereno kroz chaos-ui (2026-09-11): replacing na +125s, replika ostaje hostless (pin na
volume hosta), lease istekao ~+90s; recover → ista replika nazad na isti host active za 4s
i lease ponovo uzet.

## Regularni scenariji (bez chaosa)

### 7. Scale up / down

- `conductor scale us-east-1=4 -s web` → placer dodaje replike uz anti-affinity
  (širi po hostovima pre nego što duplira)
- `conductor scale us-east-1=2 -s web` → višak ide u `draining`, reap posle
  `drain_seconds`; traffic pointer se NE dira (scale-down nije rollout)

Izmereno: 2→4 active+healthy za **8s** (spread na 3 hosta); 4→2: draining na
+9s, reaped na +12s (drain window 10s). Kroz chaos-ui (2026-09-11): 2→4 za 5s, 4→2 za 11s.

### 8. Novi deploy — blue/green

- izmeni spec/image pa `conductor up -s web` → v2 replike se dižu paralelno sa
  v1; kad su sve healthy, traffic switch je atomski batch (SetServedRevision +
  drain starih u istoj transakciji); v1 replike se drain-uju pa reap-uju
- pad v2 (crash/stall pre nego što postane healthy) → progress deadline obara
  rollout kao failed, v1 ostaje da služi

Izmereno: `up` v2 → v2 current (2 active) i v1 reaped za **12s**. Kroz chaos-ui
(2026-09-11): deploy forma → v2 served i v1 reaped za 21-22s (drain 10s).

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
2026-09-11: deploy→active+lease 6s; recreate: stara draining → reaped +13s, nova
scheduling +16s, active + lease +18s.

### 11. Volume resize — grow-only, engine je gate za prostor

Zahtev: `conductor volume update --size N` mora da poraste disk uživo bez
restarta replike (kao Railway live resize); shrink ne postoji; zahtev koji host
ne može da primi ne sme da završi u `failed` nego čeka dok se prostor ne pojavi.

Model: CLI menja samo `desired_size_bytes`. Engine svaki tick gleda drift
(`desired > observed`) i, ako ledger hosta ima mesta (grow sme da potroši ceo
`DiskReserve`, nova plasiranja ne smeju), flipuje status `attached → resizing`.
Tek taj status otključava novu veličinu na downlinku (`volumeTargetSize`), pa
agent nikad ne raste disk koji host ne drži. Agent javlja `VolumeObservation`
sa stvarnom veličinom; kad `observed >= desired` engine vraća `resizing →
attached`. Oba flipa su po jedan red u sopstvenoj tx sa SQL predikatom
(`MarkVolumeResizing`/`MarkVolumeAttached`), izgubljena trka = drop, sledeći
tick odlučuje ponovo.

- postavka kao u 9 (stateful `pg`, `volume add --size 2`, `up`)
- `conductor volume update --mount /var/lib/postgresql/data --size 4 -s pg`
  → CLI kaže "host has room"; engine `resizing` na sledećem ticku; agent
  naraste disk u jednom ticku i javi; engine `attached`
- `--size 100` na hostu sa 80GB (budžet 64GiB) → CLI kaže "host is short
  36GiB … waits"; `volume list` pokazuje `SIZE 100GiB / ON DISK 4GiB /
  attached (grow waiting for host space)`; ništa se ne dešava koliko god tickova
- povlačenje: `--size 4` (= on disk) → "pending grow withdrawn"; pod on-disk
  veličinu CLI odbija (`grow-only`)
- prostor se pojavi (drugi volume ode sa hosta, ili operator doda disk:
  `update hosts set disk_bytes=…`) → engine sam odobri, bez akcije operatera

Izmereno (2026-09-14, agentsim tick 1s, reconcile 2s): update 10:17:05 →
`resizing` 10:17:06 → agent 4GiB 10:17:06 → `attached` 10:17:07 (**2s**).
Čekanje na prostor: 3 ticka bez promene, status label vidljiv u `volume list`.
Host 80→200GiB 10:17:55 → `resizing` +2s → `attached` na 100GiB +5s. Replika
`active|healthy|restart_count=0` ceo period, lease netaknut.

Chaos `volume_stall_resize <volume>`: engine odobri (`resizing`), agent nikad ne
javi novu veličinu → volume stoji `resizing`, `ON DISK` zaostaje; `volume_heal`
→ agent naraste i engine settle-uje za 2s. Nema timeout-a ni `failed`: stalled
resize je vidljiv drift za čoveka, ne presuda za automatiku (isti stav kao 4).

Poznata rupa: dve uzastopne `update` dok je volume već `resizing` ne prolaze
ponovo kroz disk gate (status je već odobren). Resize je za sad CLI-only —
chaos-ui ne prikazuje volumene.

### 10. Operator akcije

- `cordon` (UI → apiserver): host ostaje da služi postojeće, ne dobija novo
- `drain` (UI → apiserver): danas isto što i cordon (placer ga preskače), replike
  ostaju gde su — evakuacija još nije implementirana u reconcileru (TODO);
  `uncordon` (samo API, `POST /v1/hosts/{id}/uncordon`) vraća i cordoned i
  draining host u `ready`
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
pregaziti. Curl ekvivalent: `POST :7780/chaos` sa istim `action` poljem. Tranzicije gledaj na Topology tabu i u Logs
(`watchdog -> stale hosts out of scheduling`, `watchdog -> host down`,
`reconcile -> rule fired`).
