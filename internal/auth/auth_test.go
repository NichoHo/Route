package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
)

const testToken = "test-api-token-0123456789"

func TestBearer(t *testing.T) {
	a := New(testToken)
	cases := []struct {
		name, header string
		want         bool
	}{
		{"valid", "Bearer " + testToken, true},
		{"valid lowercase scheme", "bearer " + testToken, true},
		{"missing", "", false},
		{"wrong token", "Bearer nope", false},
		{"no scheme", testToken, false},
		{"basic scheme", "Basic " + testToken, false},
		{"empty token", "Bearer ", false},
		{"prefix of token", "Bearer " + testToken[:10], false},
		{"token plus suffix", "Bearer " + testToken + "x", false},
	}
	for _, c := range cases {
		r, _ := http.NewRequest("POST", "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		if _, ok := a.Bearer(r); ok != c.want {
			t.Errorf("%s: got %v, want %v", c.name, ok, c.want)
		}
	}
}

func TestCourierToken(t *testing.T) {
	a := New(testToken)
	tok := a.CourierToken("courier-1")

	if !a.VerifyCourier("courier-1", tok) {
		t.Fatal("issued token must verify")
	}
	if a.VerifyCourier("courier-2", tok) {
		t.Fatal("token must not verify for another courier id")
	}
	if a.VerifyCourier("courier-1", "") || a.VerifyCourier("courier-1", tok[:len(tok)-1]) {
		t.Fatal("empty or truncated token must not verify")
	}
	if New("a-different-api-token-xyz").VerifyCourier("courier-1", tok) {
		t.Fatal("token must not verify under a different API token")
	}
	if a.VerifyCourier("bad id!", a.CourierToken("bad id!")) {
		t.Fatal("invalid ids must not verify even with a matching HMAC")
	}
}

func TestValidCourierID(t *testing.T) {
	for _, id := range []string{"c1", "courier-12", "a_b.C", strings.Repeat("a", MaxCourierIDLen)} {
		if !ValidCourierID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "a:b", "a b", "a\nb", "é", strings.Repeat("a", MaxCourierIDLen+1)} {
		if ValidCourierID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func TestCourierCredentialsRoundTrip(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), CourierMetadata("c1", "tok"))
	id, tok, ok := CourierCredentials(ctx)
	if !ok || id != "c1" || tok != "tok" {
		t.Fatalf("got %q %q %v", id, tok, ok)
	}
	if _, _, ok := CourierCredentials(context.Background()); ok {
		t.Fatal("no metadata must not yield credentials")
	}
	noBearer := metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetaCourierID, "c1", MetaAuthorization, "tok"))
	if _, _, ok := CourierCredentials(noBearer); ok {
		t.Fatal("authorization without Bearer scheme must be rejected")
	}
}
