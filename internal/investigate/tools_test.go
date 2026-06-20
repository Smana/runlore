package investigate

import "testing"

func TestParseFindings(t *testing.T) {
	args := `{"confidence":0.82,"root_causes":[
	  {"summary":"chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff","confidence":0.82,"suggested_action":"flux rollback hr/harbor","reversible":true,"evidence":["pg_up=0","migration lock timeout"]}
	],"unresolved":["why the migration lock never released"]}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Confidence != 0.82 || len(inv.RootCauses) != 1 || len(inv.Unresolved) != 1 {
		t.Fatalf("unexpected investigation: %+v", inv)
	}
	rc := inv.RootCauses[0]
	if rc.Confidence != 0.82 || rc.SuggestedAction != "flux rollback hr/harbor" || !rc.Reversible || len(rc.Evidence) != 2 {
		t.Fatalf("unexpected root cause: %+v", rc)
	}
}
