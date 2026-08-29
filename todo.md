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

- [ ] mapiranje intent → tx pozivi u `WithReconcileTx`: create (CreateReplica + AssignReplicaHost CAS + lease za
      stateful), assign_host, drain, destroy (+ ReleaseVolumeLease), fail, complete
- [ ] blue/green: traffic-switch drain batch u istom tx zove `SetServedRevision` (vidi komentar `actuator.go:26-32`);
      scale-down drain served revizije NE dira pointer — treba signal na Intent-u
- [ ] `ErrConflict` = ne-greška: drop, sledeći tick self-heal
- [ ] unit testovi nad stub store-om (koji tx pozivi za koji intent)
- [ ] promovisati `scenarios_test.go` u prave end-to-end (skinuti TODO na `scenarios_test.go:11`)

## 3. Sensor

- [ ] observation loop u `sensor.go`: heartbeats, replica observations, stale hosts → MarkHostDown
- [ ] volume observed size
- [ ] testovi: stale host → down → reconciler re-place putanja
- [ ] integracioni test punog loop-a: sensor → snapshot → reconciler → actuator, jedan rollout kraj-na-kraj

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

- supervisor retry (TODO `supervisor.go:10`)
- v2 WASM plugin boundary (v2.md)
