package migrate

import "testing"

func TestIncompatibleMarker(t *testing.T) {
	if !incompatible("-- Retrait de l'ancienne fonction.\n  -- lattis:incompatible  \nDROP FUNCTION lattis.x();") {
		t.Fatal("marque non lue")
	}
	if incompatible("-- lattis:incompatible plus tard\nSELECT 1;") || incompatible("SELECT '-- lattis:incompatible';") {
		t.Fatal("marque lue à tort")
	}
	all, err := All()
	if err != nil || len(all) == 0 {
		t.Fatal(err)
	}
}
