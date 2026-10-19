package webapp

import "testing"

// UID des moteurs : stable par utilisateur, le suivant libre sinon, aucun
// quand la plage est pleine.
func TestPickUID(t *testing.T) {
	used := map[int]string{}
	a, ok := pickUID(200000, 3, used, "alice")
	if !ok || a < 200000 || a > 200002 {
		t.Fatalf("UID : %d %v", a, ok)
	}
	if again, _ := pickUID(200000, 3, used, "alice"); again != a {
		t.Fatal("UID instable")
	}
	used[a] = "bob"
	b, ok := pickUID(200000, 3, used, "alice")
	if !ok || b == a {
		t.Fatalf("UID occupé repris : %d", b)
	}
	for uid := 200000; uid <= 200002; uid++ {
		used[uid] = "plein"
	}
	if _, ok := pickUID(200000, 3, used, "alice"); ok {
		t.Fatal("plage pleine acceptée")
	}
}
