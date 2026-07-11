# Fuzz testiranje u conductor-u

## Zašto uopšte može da radi ovde

`Reconciler.Reconcile` je namerno čist: snapshot ulazi, intenti izlaze, bez
pristupa storage-u. To je tačno oblik koji fuzzing traži — funkcija koju možeš
da bombarduješ nasumičnim ulazima i proveravaš invarijante nad izlazom, bez
mock-ovanja baze.

Dve odvojene prilike:

## 1. Property-based fuzzing rekonsajlera (glavni dobitak)

Sirovi `go test -fuzz` mutira bajtove, što je nezgodno za strukturirano stanje
kao `stateSnapshot`. Idiomatski pristup: **property-based testiranje sa
`pgregory.net/rapid`**, uvezano u Go-ov nativni fuzzer preko `rapid.MakeFuzz`.

Napišeš generatore za nasumične snapshotove (nasumični slotovi, replike sa
nasumičnim `Current`/`Phase`/`RestartCount`, desired stanja koja možda pokrivaju
te slotove a možda ne), pa asertuješ invarijante koje moraju da važe za *bilo
koji* ulaz:

- **Zakon particije**: svaka replika iz snapshota se pojavljuje u tačno jednoj
  grupi, tačno jednom (target xor outgoing). Ovo je tačno klasa buga koju je
  orphan-slot fix rešio — fuzzer bi našao "replike u slotovima koje nijedan
  deployment ne deklariše tiho nestaju" bez da se iko setio scenarija.
- **Nema izmišljenih slotova**: svaki `Intent.Group` se mapira nazad na slot
  prisutan u `desired` ili `replicas`.
- **Ispravnost crash guard-a**: `IntentFail` za grupu ⟺ neka current replika
  ima `RestartCount > RestartMax` *i* status ≠ failed. Već failed grupe ne
  emituju ništa (hold ponašanje iz komentara u kodu).
- **Jedno pravilo po grupi po tick-u**: nikad dva konfliktna intenta za isti
  slot/repliku.
- **Determinizam**: `Reconcile(snap)` dvaput → isti intenti. Bitno jer
  `buildReplicaGroups` iterira mapu za orphan slotove — ako redosled intenata
  ikad počne da znači nešto (npr. actuator ih primenjuje sekvencijalno), ovo
  hvata nedeterminizam prvog dana.

Skica:

```go
func FuzzReconcile(f *testing.F) {
	f.Fuzz(rapid.MakeFuzz(func(t *rapid.T) {
		snap := genSnapshot().Draw(t, "snap")
		intents := NewReconciler().Reconcile(snap)

		// zakon particije
		seen := map[uuid.UUID]int{}
		for _, g := range buildReplicaGroups(snap) {
			for _, r := range append(g.TargetReplicas, g.OutgoingReplicas...) {
				seen[r.ID]++
			}
		}
		for _, r := range snap.replicas {
			if seen[r.ID] != 1 {
				t.Fatalf("replika %s se pojavljuje %d puta", r.ID, seen[r.ID])
			}
		}
		// ... crash-guard iff, provera porekla slotova za intente
	}))
}
```

Generator je glavna investicija (~40 linija): izvučeš mali skup slotova, pa
replike pristrasno vezuješ za te slotove uz povremeno izmišljanje orphan
slotova — ta pristrasnost je ono što pogađa zanimljiva preklapanja umesto
čistog šuma.

## 2. Byte-level fuzzing parsera (jeftino, klasične mete)

`internal/config`, `internal/deployspec`, `internal/spec` parsiraju spoljni
ulaz (TOML itd.). Za njih idu obični nativni fuzz targeti:

```go
f.Fuzz(func(t *testing.T, data []byte) { Parse(data) })
```

Asertuje se: nema panic-a, plus round-trip (`parse → encode → parse` jednako)
gde ima smisla. Korpus se seed-uje sa `example/config.toml`. Mali trud, a to je
kod najizloženiji neprijateljskom/đubre ulazu.

## Budući dobitak: rollout orkestrator

`TODO` u `reconciler.go` (rollout orkestrator + bin-packing) je gde se ovo
stvarno isplati. Kad intenti počnu da smeštaju replike na hostove, dodaju se
**model-based properties**: primeni intente na in-memory model, ponovo
rekonsajluj, i asertuj:

- **konvergencija** — fixpoint od nula intenata se dostiže u N tick-ova
- **kapacitet** — smeštanja nikad ne prelaze CPU/mem hosta
- **afinitet** — stateful replike zadržavaju volume/host afinitet

Rollout state machine je tačno mesto gde ručno pisanim table testovima ponestane
mašte, a fuzzeri blistaju.

## Praktično

- Lokalno: `go test -fuzz=FuzzReconcile -fuzztime=30s`
- Nađeni failure-i završavaju u `testdata/fuzz/` kao regresioni seed-ovi i
  zauvek posle toga idu pod običan `go test`
- Start: particija + crash-guard properties odmah (~sat posla nad postojećim
  kodom), lista invarijanti raste kako rollout pravila stižu
