# TODO — put do zatvorenog loop-a

Redosled: placement → actuator → sensor. Svaki korak zeleni testovi pre sledećeg.

## 1. Bin-packing (Reconciler, decide faza)

- [x] `Intent` dobija `HostID` (bez revision polja — hosts.revision je izbačen, commit čuva predikatska rezervacija;
      vidi docs/bin-pack.md)
- [x] `placeHostless(snap, intents)` — post-pass posle `planIntents`: greedy, deljeni in-memory kapacitet-ledger,
      dot-product best-fit decreasing, replacement pre create; nema mesta → drop intenta (sledeći tick)
- [x] `placeVolumes(snap)` — 3D (cpu/mem/disk), volume-budget + disk-reserve, `place_volume` intent
- [x] unit testovi za placer: ledger preko batch-a, stuck-sort, ledger anti-affinity, fallback distribucija,
      reserve blokira create ali ne replacement, scarcity flip, volume boundary/reserve/ledger putanje
- [x] skinuti TODO na `reconciler.go:77`
- [x] placement server flagovi (`-placement.*`) kroz `internal/config` + `cmd/engine`

## 2. Actuator.Apply

- [x] mapiranje intent → tx pozivi u `WithReconcileTx`: create (CreateReplica, hostless + volume bind za stateful),
      assign_host (+ AcquireVolumeLease za stateful), place_volume, drain, destroy (+ ReleaseVolumeLease), fail,
      complete (+ SetServedRevision za first-deploy/recreate)
- [x] blue/green: traffic-switch drain batch u istom tx zove `SetServedRevision`; scale-down drain NE dira pointer —
      `SwitchTraffic` signal na Intent-u (postavlja ga samo drainOutgoing)
- [x] `ErrConflict` = ne-greška: drop, sledeći tick self-heal (tx po intentu; switch batch je jedan tx po slotu)
- [x] predikatska rezervacija u `ReserveReplicaOnHost`: WHERE nosi invarijantu (host ready + kapacitet, sum živih
      replika, failed ne broji) — 0 rows = ErrConflict = stvarno ne staje; integracioni test `storage/reconcile_test.go`
- [x] unit testovi nad stub store-om (koji tx pozivi za koji intent) — `actuator_test.go`
- [x] promovisati scenarije u prave end-to-end — `e2e_test.go` (in-memory store, pravi tick + sensor);
      `scenarios_test.go` ostaje decision-level (fake clock za drain/deadline pravila)

## 3. Sensor

- [x] observation loop u `sensor.go`: heartbeats, replica observations, stale hosts → MarkHostDown (atomski CTE:
      host notready + replike hostless/replacing → re-place putanja; observation guard drži replacing — zombi
      agent ne može da vaskrsne oslobođenu repliku)
- [x] volume observed size (`RecordVolumeObservedSize`)
- [x] testovi: stale host → down → reconciler re-place (`e2e_test.go` + integracioni `storage/sensor_test.go`)
- [x] integracioni test punog loop-a: sensor → snapshot → reconciler → actuator, rollout kraj-na-kraj
      (`TestE2EBlueGreenRollout`, `TestE2EStatefulRecreateKeepsSingleWriter`)

## 4. Simulirani host agent (chaos tačka)

Niko trenutno ne pomera stvarni svet: actuator piše `phase=scheduling`, sensor čita observacije — ali ko ih pravi?
Simulirani host agent je taj most, i namerno je idealna chaos tačka: umesto pravog containerd-a, agent kome
scenario kaže kako da laže/umire.

- [ ] per-host loop: čita svoje replike (`host_id=ja`, `phase=scheduling`) → glumi start → health_check → healthy;
      `draining` → graciozno gašenje; hrani Sensor (`RecordReplicaObservation`, `RecordHostHeartbeat`)
- [ ] chaos kvake (za chaos-ui): ubij host (prestani heartbeat), ubij repliku (failed + exit reason), crash-loop
      (restart_count++), zaglavi health probe (nikad healthy → progress deadline putanja)
- [ ] scenario konfiguracija: determinističke skripte, bez wall-clock random-a — da e2e testovi budu ponovljivi

## Posle

- [x] volume lease renewal: healthy observacija kroz Sensor obnavlja lease (`RenewVolumeLease`, holder-keyed —
      zombijeva obnova ne dira preuzeti lease); unhealthy NE obnavlja → istek oslobađa volume za failover
- [x] supervisor retry: restart budžet 3 back-to-back crash-a, stabilan run (≥1min) resetuje budžet;
      engine+sensor se restartuju zajedno (`supervisor.go` + testovi)
- [ ] agent transport v1 — plain HTTP+JSON: uplink POST (heartbeat, observacije → Sensor fasada, handleri su
      tanki), downlink poll `ListReplicasByHost` preko keep-alive (level-triggered: pun spisak, ne delte)
- [ ] agent transport v2 — gRPC bidi `AgentSession` stream: uplink i downlink na jednoj konekciji, `.proto`
      ugovor + codegen za obe strane, downlink push na Postgres LISTEN/NOTIFY umesto poll-a. Pravila prenosa
      ostaju ista: preko streama uvek celo stanje hosta, pun snapshot na rekonekt, periodični resync; konekcija
      NIJE heartbeat (eksplicitni heartbeat sa server timestampom ostaje izvor istine o liveness-u)
- v2 WASM plugin boundary (v2.md)
