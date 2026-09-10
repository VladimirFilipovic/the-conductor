# agentsim — gRPC transport + mock agenti + chaos CLI

Odluke: agentsim = poseban proces (Fleet), paket van `internal/` (`agentsim/`); chaos ide preko
HTTP control API-ja na agentsim-u (`conductor chaos` ga gađa; chaos-ui kasnije prelazi na isti);
gRPC gateway živi u engine procesu, pod istim supervisorom kao engine+sensor petlje.

Pravila prenosa (iz todo.md): preko streama uvek CELO stanje hosta, pun snapshot na
(re)konekt, periodični resync; konekcija NIJE heartbeat — eksplicitni heartbeat sa
server timestampom ostaje izvor istine.

## Koraci

- [x] 1. proto ugovor (`proto/agentpb/agent.proto`): bidi `Session` stream (uplink
       Hello/Heartbeat/Observation, downlink HostState = pun spisak replika hosta) + `ListHosts`
       (da agentsim ne dira bazu)
- [x] 2. codegen (protoc-gen-go + protoc-gen-go-grpc), generisan kod commitovan uz .proto
- [x] 3. migracija 00001 u mestu: `notify_replicas_changed` trigger na `replicas` →
       `pg_notify('replicas_changed', host_id)`; UPDATE notifikuje i OLD i NEW host, filtrira
       na promenu host_id/phase (healthy/restart_count agenta ne zanimaju — on ih proizvodi)
- [x] 4. storage: `ListenReplicaChanges` (internal/storage/notify.go) — dedikovana pgx konekcija
       (LISTEN je session-scoped, ne može kroz pool), reconnect petlja, payload = ključ ne podatak
- [x] 5. gateway u engine procesu (`internal/engine/gateway.go`): Session → Sensor fasada;
       downlink: latest-wins mailbox po sesiji, keš poslednjeg poslatog stanja (proto.Equal dedupe),
       osvežavanje na NOTIFY re-SELECT-om, read-through na konekt, force resync na 60s ticker
- [x] 6. wiring: `conductor engine -grpc.addr` (default :7443), gateway kao treća komponenta u
       supervisor errgroup (restartuje se zajedno sa engine+sensor)
- [x] 7. `agentsim/` paket (van internal): `Agent` objekat — lifecycle Start/Stop, reconnect petlja,
       fake kontejneri starting→health_check→active, draining→graciozno gašenje, apsent→uklanjanje;
       chaos kvake: KillHost (ćuti, stream namerno ostaje otvoren — konekcija≠heartbeat),
       RecoverHost, CrashReplica (terminalno failed), CrashLoop (restart_count raste svaki tick),
       StallHealth (nikad healthy), Heal
- [x] 8. agentsim Fleet: discovery preko gRPC ListHosts (+ 30s re-discovery za nove hostove),
       spawn 1 agent po hostu; HTTP control API (GET /agents, POST /chaos — isti oblik kao
       chaos-ui /api/chaos, da UI kasnije pređe na njega umesto SQL varanja)
- [x] 9. `cmd`: `conductor agentsim` (flagovi gateway.addr/control.addr/tick) +
       `conductor chaos agents|kill-host|recover-host|crash|crashloop|stall|heal`
- [x] 10. testovi (bufconn, -race): `TestGatewaySessionProtocol` (hello→snapshot, observation→store,
        heartbeat→host, notify→push, cache dedupe) i `TestGatewayFullLoopWithAgent` (pravi
        agentsim.Agent konvergira deployment kroz žicu, pa chaos crash-loop obara u failed);
        memStore dobio mutex (konkurentni agent + engine tick)
- [x] 11. build + go vet + svi testovi zeleni (203, -race)
- [x] 12. artifact: ideja implementacije, bitni snippeti, chaos scenariji za testiranje

## Pokretanje

```
make migrate-fresh          # trigger je u 00001, menjan u mestu
conductor engine            # reconcile + sensor + gRPC gateway na :7443
conductor agentsim          # flota mock agenata + control API na :7780
conductor chaos agents      # pregled flote
```
