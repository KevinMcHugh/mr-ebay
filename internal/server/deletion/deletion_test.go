package deletion_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
	"github.com/KevinMcHugh/mr-ebay/internal/server"
	"github.com/KevinMcHugh/mr-ebay/internal/server/deletion"
)

func TestChallenge(t *testing.T) {
	ctx := context.Background()
	e := deletion.ChallengeEndpoint{Token: "token_0123456789abcdef0123456789abcdef", URL: "https://app.example/api/ebay/account-deletion"}
	got, err := server.Run(ctx, deletion.ChallengeStore{}, e, apigen.EbayDeletionChallengeRequestObject{Params: apigen.EbayDeletionChallengeParams{ChallengeCode: "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("abc123" + e.Token + e.URL))
	if want := hex.EncodeToString(sum[:]); got.(apigen.EbayDeletionChallenge200JSONResponse).ChallengeResponse != want {
		t.Fatalf("got %+v, want %s", got, want)
	}

	if _, err := server.Run(ctx, deletion.ChallengeStore{}, deletion.ChallengeEndpoint{URL: e.URL}, apigen.EbayDeletionChallengeRequestObject{Params: apigen.EbayDeletionChallengeParams{ChallengeCode: "abc123"}}); !errors.Is(err, deletion.ErrNotConfigured) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := server.Run(ctx, deletion.ChallengeStore{}, e, apigen.EbayDeletionChallengeRequestObject{}); !errors.Is(err, app.ErrBadInput) {
		t.Fatalf("no challenge code: %v", err)
	}
}

// forgetter records whom it was asked to forget.
type forgetter struct{ forgot []string }

func (f *forgetter) ForgetSeller(_ context.Context, seller string) (int64, error) {
	f.forgot = append(f.forgot, seller)
	return 2, nil
}

// Notice builds a notice as eBay sends one.
func Notice(topic, username string) []byte {
	b, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"topic": topic, "schemaVersion": "1.0", "deprecated": false},
		"notification": map[string]any{
			"notificationId": "n-1", "eventDate": "2026-11-25T00:00:00.000Z", "publishDate": "2026-11-25T00:00:01.000Z", "publishAttemptCount": 1,
			"data": map[string]any{"username": username, "userId": "u-1", "eiasToken": "e-1"},
		},
	})
	return b
}

func notify(fake *ebaytest.Server, eb ebay.KeyFetcher, store *forgetter, body []byte, sig string) error {
	raw := apigen.AccountDeletionNotice(body)
	_, err := server.Run(context.Background(), deletion.Deps(store, eb), deletion.NotifyEndpoint{},
		apigen.EbayAccountDeletionRequestObject{Params: apigen.EbayAccountDeletionParams{XEBAYSIGNATURE: sig}, Body: &raw})
	return err
}

func TestNotify(t *testing.T) {
	fake := ebaytest.New(t)
	eb := ebay.New(ebay.Config{BaseURL: fake.URL, TokenURL: fake.Connector(t)})
	body := Notice(deletion.Topic, "seller_1")

	t.Run("a signed notice forgets the seller", func(t *testing.T) {
		store := &forgetter{}
		if err := notify(fake, eb, store, body, fake.Sign(body)); err != nil {
			t.Fatal(err)
		}
		if len(store.forgot) != 1 || store.forgot[0] != "seller_1" {
			t.Fatalf("forgot %v", store.forgot)
		}
	})

	t.Run("the key is cached", func(t *testing.T) {
		before := fake.KeyCalls()
		if err := notify(fake, eb, &forgetter{}, body, fake.Sign(body)); err != nil {
			t.Fatal(err)
		}
		if fake.KeyCalls() != before {
			t.Fatalf("fetched the key again: %d calls, was %d", fake.KeyCalls(), before)
		}
	})

	for name, tc := range map[string]struct {
		body []byte
		sig  string
	}{
		"a changed body":       {Notice(deletion.Topic, "someone_else"), fake.Sign(body)},
		"no signature":         {body, ""},
		"a signature not b64":  {body, "%%%"},
		"a signature not JSON": {body, base64.StdEncoding.EncodeToString([]byte("nope"))},
		"another algorithm": {body, base64.StdEncoding.EncodeToString(func() []byte {
			b, _ := json.Marshal(map[string]string{"alg": "rsa", "kid": ebaytest.KeyID, "signature": "AA==", "digest": "SHA256"})
			return b
		}())},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			store := &forgetter{}
			if err := notify(fake, eb, store, tc.body, tc.sig); !errors.Is(err, deletion.ErrBadSignature) {
				t.Fatalf("got %v, want ErrBadSignature", err)
			}
			if len(store.forgot) != 0 {
				t.Fatalf("forgot %v", store.forgot)
			}
		})
	}

	t.Run("an unknown key is eBay's to retry", func(t *testing.T) {
		var s deletion.Signature
		raw, _ := base64.StdEncoding.DecodeString(fake.Sign(body))
		_ = json.Unmarshal(raw, &s)
		s.Kid = "no-such-key"
		b, _ := json.Marshal(s)
		if err := notify(fake, eb, &forgetter{}, body, base64.StdEncoding.EncodeToString(b)); !errors.Is(err, app.ErrUpstream) {
			t.Fatalf("got %v, want ErrUpstream", err)
		}
	})

	t.Run("another topic is acknowledged and ignored", func(t *testing.T) {
		other := Notice("SOMETHING_ELSE", "seller_1")
		store := &forgetter{}
		if err := notify(fake, eb, store, other, fake.Sign(other)); err != nil {
			t.Fatal(err)
		}
		if len(store.forgot) != 0 {
			t.Fatalf("forgot %v", store.forgot)
		}
	})

	t.Run("an unconfigured eBay client is eBay's to retry", func(t *testing.T) {
		if err := notify(fake, ebay.New(ebay.Config{}), &forgetter{}, body, fake.Sign(body)); !errors.Is(err, app.ErrUpstream) {
			t.Fatalf("got %v, want ErrUpstream", err)
		}
	})
}
