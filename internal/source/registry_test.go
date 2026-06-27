package source

import (
	"context"
	"net/http"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

type fakeWebhook struct{}
func (fakeWebhook) Decode(_ []byte, _ http.Header) (DecodeResult, error) { return DecodeResult{}, nil }

func TestRegisterAndBuildEnabled(t *testing.T) {
	resetForTest() // clears the package registry between tests
	Register(Descriptor{Name: "fake", Kind: Webhook, Admission: MatchGated, Path: "/webhook/fake",
		Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})
	built, err := BuildEnabled(Deps{Cfg: &config.Config{}})
	if err != nil { t.Fatal(err) }
	if len(built) != 1 || built[0].Desc.Name != "fake" {
		t.Fatalf("got %+v", built)
	}
	if _, ok := built[0].Impl.(WebhookSource); !ok { t.Fatal("impl is not a WebhookSource") }
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "dup", Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})
	defer func() { if recover() == nil { t.Fatal("expected panic on duplicate") } }()
	Register(Descriptor{Name: "dup", Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})
}

func TestBuildEnabledSkipsNilImpl(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "off", Build: func(Deps) (any, error) { return nil, nil }})
	built, err := BuildEnabled(Deps{Cfg: &config.Config{}})
	if err != nil { t.Fatal(err) }
	if len(built) != 0 { t.Fatalf("expected disabled source skipped, got %d", len(built)) }
}

func TestBuildEnabledFailFast(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "bad", Build: func(Deps) (any, error) { return nil, context.DeadlineExceeded }})
	if _, err := BuildEnabled(Deps{Cfg: &config.Config{}}); err == nil {
		t.Fatal("expected build error to propagate")
	}
}
