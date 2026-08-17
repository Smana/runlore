// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"

	// These blank imports are what notify.Registered() can see below: a
	// notifier subpackage missing from this block is invisible to the
	// collision guard. TestEveryNotifierSubpackageIsImportedHere
	// (notifier_imports_test.go) keeps the block complete by reading the
	// internal/notify directory, so it can never fall behind
	// internal/app/serve.go's own blank-import block.
	_ "github.com/Smana/runlore/internal/notify/templated" // self-registers the "templated" notifier
	_ "github.com/Smana/runlore/internal/notify/webhook"   // self-registers the "webhook" notifier
)

// configFieldNames reflects the yaml tag of every NAMED (non-inline) field
// on cfg's type, so the set can never drift from the struct it describes the
// way a hand-maintained list could.
func configFieldNames(cfg any) map[string]bool {
	names := map[string]bool{}
	rt := reflect.TypeOf(cfg)
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue // untagged, "-", or the inline catch-all claim no name
		}
		names[name] = true
	}
	return names
}

// collidingNames returns every entry of names that fields also claims — the
// invariant config.Notify's Extra field doc comment documents: a
// notify.<name> block for a name a named struct field already claims lands
// on that field instead of the inline Extra map, silently shadowing the
// notifier's own config.
func collidingNames(names []string, fields map[string]bool) []string {
	var out []string
	for _, n := range names {
		if fields[n] {
			out = append(out, n)
		}
	}
	return out
}

// TestCollidingNames_detectsSyntheticCollision pins collidingNames' own
// detection logic against a synthetic struct and name list, independent of
// whatever config.Notify and notify.Registered() actually contain today —
// so a regression in the DETECTION logic itself, not just a future collision
// in real data, fails this test.
func TestCollidingNames_detectsSyntheticCollision(t *testing.T) {
	type fake struct {
		Slack string `yaml:"slack"`
		Extra string `yaml:",inline"`
	}
	fields := configFieldNames(fake{})

	got := collidingNames([]string{"slack", "webhook"}, fields)
	if len(got) != 1 || got[0] != "slack" {
		t.Fatalf("collidingNames(...) = %v, want [slack]", got)
	}

	got = collidingNames([]string{"webhook", "templated"}, fields)
	if len(got) != 0 {
		t.Fatalf("collidingNames(...) = %v, want none (no name collides)", got)
	}
}

// builtInPkgPath is the import path of package notify itself — the ORIGIN of
// the two notifiers implemented directly there (slack.go, matrix.go). Those
// two are deliberately registered under the SAME name as their own dedicated
// config.Notify field (Slack/Matrix), because both Build closures read
// cfg.Notify.Slack / cfg.Notify.Matrix by name. That pairing is the reason
// those fields exist, not the collision Extra's doc comment warns about. Any
// OTHER registered notifier — every drop-in, registered from a subpackage and
// landing in Notify.Extra — must NOT share a name with any named
// config.Notify field.
//
// The exemption keys on this path rather than on the pair of names
// {"slack", "matrix"}: a name-keyed exemption skips whoever CLAIMS the name,
// which is exactly the collision worth catching (see
// TestBuiltInExemptionKeysOnOrigin).
const builtInPkgPath = "github.com/Smana/runlore/internal/notify"

// registeringPackage returns the import path of the package that DEFINED d's
// Build closure — where the notifier came from. That is a fact about the
// binary, established by the compiler, not a string the registrant chose: a
// drop-in cannot become built-in by naming itself "slack".
//
// It returns "" when no origin can be established (a nil Build), which never
// matches builtInPkgPath — an unknown origin is not an exemption.
func registeringPackage(d notify.Descriptor) string {
	fn := runtime.FuncForPC(reflect.ValueOf(d.Build).Pointer())
	if fn == nil {
		return ""
	}
	// "github.com/Smana/runlore/internal/notify/webhook.init.0.func1": the
	// package path runs up to the first "." AFTER the last "/", since a path
	// element may not contain "." before that point but the symbol always does.
	name := fn.Name()
	slash := strings.LastIndex(name, "/")
	dot := strings.Index(name[slash+1:], ".")
	if dot < 0 {
		return name
	}
	return name[:slash+1+dot]
}

// dropInNames returns the registered name of every notifier that did NOT come
// from package notify itself — every drop-in, whose config block lands in
// Notify.Extra and which must therefore not share a name with a named
// config.Notify field.
func dropInNames(ds []notify.Descriptor) []string {
	var out []string
	for _, d := range ds {
		if registeringPackage(d) == builtInPkgPath {
			continue // slack.go / matrix.go: deliberately paired with their own field
		}
		out = append(out, d.Name)
	}
	return out
}

// TestBuiltInExemptionKeysOnOrigin pins WHAT the built-in exemption is allowed
// to key on. Slack and Matrix are exempt because of where they come from —
// package notify itself, where the Build closure reads the dedicated
// cfg.Notify.Slack / cfg.Notify.Matrix field that is the whole reason the
// pairing exists. They are NOT exempt because of the string they registered
// under, which any drop-in can also choose.
//
// A drop-in calling itself "slack" is the strongest form of the collision this
// file exists to catch: its notify.slack block lands on the named SlackNotify
// field, so it sees no config of its own at all. An exemption keyed on the
// name waves exactly that case through.
func TestBuiltInExemptionKeysOnOrigin(t *testing.T) {
	impostor := notify.Descriptor{
		Name:  "slack",
		Build: func(notify.Deps) (providers.Notifier, error) { return nil, nil },
	}
	if got := dropInNames([]notify.Descriptor{impostor}); len(got) != 1 || got[0] != "slack" {
		t.Fatalf("dropInNames(a descriptor registered from outside package notify as %q) = %v, want [slack] — "+
			"the exemption must key on the notifier's origin, not on the name it chose", impostor.Name, got)
	}

	// The real registry: the two built-ins stay exempt, every drop-in is
	// reported. Without this half, keying on origin could exempt nothing at all
	// and the test above would still pass.
	got := map[string]bool{}
	for _, n := range dropInNames(notify.Registered()) {
		got[n] = true
	}
	for _, builtIn := range []string{"slack", "matrix"} {
		if got[builtIn] {
			t.Errorf("%q is implemented in package notify and paired with its own config field, so it must stay exempt", builtIn)
		}
	}
	for _, dropIn := range []string{"webhook", "templated"} {
		if !got[dropIn] {
			t.Errorf("%q is registered from its own subpackage and lands in Notify.Extra, so it must be checked", dropIn)
		}
	}
}

// TestRegisteredNotifierNamesDoNotCollideWithConfigFields is the actual
// guard config.go's Notify.Extra doc comment used to ask a maintainer to
// re-check by hand with `grep -rn 'Register(Descriptor{' internal/notify/`.
// That command matched only the bare Register(Descriptor{...}) call form
// used by internal/notify/slack.go and internal/notify/matrix.go — it missed
// internal/notify/webhook and internal/notify/templated, which register
// through the qualified notify.Register(notify.Descriptor{...}) form, so it
// silently checked half the registry.
//
// This test reflects over notify.Registered() instead — the SAME
// enumeration notify.BuildEnabled uses at runtime to build every registered
// notifier — so it can never miss a registration regardless of which call
// form a notifier package uses. A collision here means a notify.<name>
// config block for a drop-in notifier would land on a named config.Notify
// field instead of Notify.Extra, silently shadowing that notifier's own
// configuration.
func TestRegisteredNotifierNamesDoNotCollideWithConfigFields(t *testing.T) {
	fields := configFieldNames(config.Notify{})

	if bad := collidingNames(dropInNames(notify.Registered()), fields); len(bad) > 0 {
		t.Fatalf("registered notifier name(s) %v collide with a named config.Notify field — "+
			"a notify.<name> block for that name would land on the field instead of Notify.Extra", bad)
	}
}
