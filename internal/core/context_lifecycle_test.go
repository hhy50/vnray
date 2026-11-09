package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/rule"
)

func TestConnectorDuplicateStartKeepsRunningContext(t *testing.T) {
	key := authkey.Generate()
	b := testBindings(t)
	svc := newSecureTestServer(t, b, key)
	original := svc.context()
	second, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Start(second); err == nil {
		t.Fatal("duplicate start succeeded")
	}
	if got := svc.context(); got != original || got.Err() != nil {
		t.Fatalf("duplicate start replaced the running context: %v", got)
	}
	session := newReportSession(t, svc, key)
	info := protocol.ClientInfo{Name: "team:g:after-duplicate-start", Hostname: "still-running"}
	reply, err := exchangeReport(t, session, info)
	if err != nil || !reply.OK {
		t.Fatalf("new client handshake after duplicate start: %+v, %v", reply, err)
	}
	stored, ok := b.LookupClient(context.Background(), info.Name)
	if !ok || stored.Config == "" {
		t.Fatal("new client report was not persisted")
	}
}

func TestCanceledControlMutationsKeepCommittedState(t *testing.T) {
	st := testStore(t)
	sc := newTestServiceConfig(st, config.DefaultServiceSettings())
	baseline := config.DefaultServiceSettings()
	baseline.PublicHost = "committed.example"
	if err := sc.Apply(context.Background(), baseline); err != nil {
		t.Fatal(err)
	}
	tls := config.VLESSTLS{Enabled: true, ServerName: "committed.example", CertPEM: "certificate", KeyPEM: "key"}
	if err := sc.SetVLESSTLS(context.Background(), tls); err != nil {
		t.Fatal(err)
	}
	rules := loadRuleSet(context.Background(), st)
	if err := rules.SaveRule(context.Background(), "guard", rule.Rule{Reject: []string{"old.example"}}); err != nil {
		t.Fatal(err)
	}
	before := sc.Current()
	storedBefore, err := st.GetServiceSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	next := sc.Current()
	next.PublicHost = "uncommitted.example"
	next.TransportEncryption = !next.TransportEncryption
	for _, mutation := range []struct {
		name  string
		apply func() error
	}{
		{"settings", func() error { return sc.Apply(ctx, next) }},
		{"tls", func() error { return sc.SetVLESSTLS(ctx, config.VLESSTLS{ServerName: "uncommitted.example"}) }},
		{"rule", func() error { return rules.SaveRule(ctx, "guard", rule.Rule{Reject: []string{"new.example"}}) }},
		{"rule-delete", func() error { return rules.DeleteRule(ctx, "guard") }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if err := mutation.apply(); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled mutation error = %v", err)
			}
			if got := sc.Current(); !reflect.DeepEqual(got, before) {
				t.Fatalf("canceled mutation published settings: %+v", got)
			}
			if got := sc.svc.transportEncryption.Load(); got != before.TransportEncryption {
				t.Fatalf("canceled mutation changed transport encryption to %v", got)
			}
			storedAfter, err := st.GetServiceSettings(context.Background())
			if err != nil || !reflect.DeepEqual(storedAfter, storedBefore) {
				t.Fatalf("canceled mutation changed persisted state: %+v, %v", storedAfter, err)
			}
			if verdict, _ := rules.Verdict("guard", "old.example"); verdict != rule.Reject {
				t.Fatalf("committed rule was lost: %v", verdict)
			}
			if verdict, _ := rules.Verdict("guard", "new.example"); verdict != rule.Pass {
				t.Fatalf("uncommitted rule was published: %v", verdict)
			}
		})
	}
}
