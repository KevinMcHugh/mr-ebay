// Package deletion is eBay's Marketplace Account Deletion contract. eBay
// enables an app's production keyset only once the app takes its notices:
// when an eBay user closes their account, eBay posts a signed notice to
// every app, and each must forget that user.
//
// Mr. eBay stores eBay users only as sellers of listings (a username, a
// feedback percentage, and the seller object inside the raw summary), so
// forgetting one scrubs those from the listings they sold. The listing
// stays, since it is about an item.
//
// Two operations, both public because eBay calls them with no credential:
//   - GET answers the challenge eBay sends when the endpoint is registered,
//     proving the endpoint is ours (ChallengeEndpoint).
//   - POST takes a notice, which its X-EBAY-SIGNATURE authenticates
//     (NotifyEndpoint).
//
// See docs/design.md.
package deletion

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
)

// Path is where the operations are mounted, under the API's base URL.
const Path = "/ebay/account-deletion"

// Topic is the only notice topic this endpoint acts on.
const Topic = "MARKETPLACE_ACCOUNT_DELETION"

var (
	// ErrNotConfigured is the challenge arriving with no verification token
	// set (EBAY_DELETION_TOKEN): a 503, since there is no answer to give.
	ErrNotConfigured = errors.New("eBay account deletion is not configured: set EBAY_DELETION_TOKEN")
	// ErrBadSignature is a notice whose X-EBAY-SIGNATURE is missing,
	// malformed, or does not verify: a 412, which is what eBay expects.
	ErrBadSignature = errors.New("the notice's signature does not verify")
)

// -----------------------------------------------------------------------------
// Challenge
// -----------------------------------------------------------------------------

// ChallengeStore is what the challenge reads through: nothing, since its
// answer comes from the endpoint's configuration alone.
type ChallengeStore struct{}

// ChallengeEndpoint answers eBay's registration challenge. Token is the
// verification token entered in eBay's developer portal, and URL the
// endpoint's public URL exactly as entered there: both go into the answer.
type ChallengeEndpoint struct {
	Token string
	URL   string
}

// ChallengeView is the answer eBay checks.
type ChallengeView struct {
	ChallengeResponse string `json:"challengeResponse"`
}

func (e ChallengeEndpoint) Interact(_ context.Context, _ ChallengeStore, req apigen.EbayDeletionChallengeRequestObject) (string, error) {
	if e.Token == "" {
		return "", ErrNotConfigured
	}
	if req.Params.ChallengeCode == "" {
		return "", fmt.Errorf("%w: challenge_code is empty", app.ErrBadInput)
	}
	return req.Params.ChallengeCode, nil
}

// Build hashes the challenge, the token and the URL, in that order.
func (e ChallengeEndpoint) Build(code string) ChallengeView {
	sum := sha256.Sum256([]byte(code + e.Token + e.URL))
	return ChallengeView{ChallengeResponse: hex.EncodeToString(sum[:])}
}

func (ChallengeEndpoint) Render(vm ChallengeView) apigen.EbayDeletionChallengeResponseObject {
	return apigen.EbayDeletionChallenge200JSONResponse{ChallengeResponse: vm.ChallengeResponse}
}

// -----------------------------------------------------------------------------
// Notice
// -----------------------------------------------------------------------------

// ForgetStore is the store a notice writes through.
type ForgetStore interface {
	ForgetSeller(ctx context.Context, seller string) (int64, error)
}

// NotifyDeps is what a notice needs: eBay, for the key that verifies it,
// and the store, to forget the user. Deps joins them.
type NotifyDeps interface {
	ForgetStore
	ebay.KeyFetcher
}

// Deps joins a store and an eBay client into a notice's NotifyDeps.
func Deps(store ForgetStore, eb ebay.KeyFetcher) NotifyDeps { return deps{store, eb} }

type deps struct {
	ForgetStore
	ebay.KeyFetcher
}

// NotifyEndpoint verifies a notice and forgets the user it names.
type NotifyEndpoint struct{}

// Forgotten is what a notice did: how many listings it scrubbed.
type Forgotten struct {
	Listings int64 `json:"listings"`
}

type notice struct {
	Metadata struct {
		Topic string `json:"topic"`
	} `json:"metadata"`
	Notification struct {
		NotificationID string `json:"notificationId"`
		Data           struct {
			Username string `json:"username"`
		} `json:"data"`
	} `json:"notification"`
}

func (NotifyEndpoint) Interact(ctx context.Context, d NotifyDeps, req apigen.EbayAccountDeletionRequestObject) (Forgotten, error) {
	if req.Body == nil || len(*req.Body) == 0 {
		return Forgotten{}, fmt.Errorf("%w: body required", app.ErrBadInput)
	}
	body := []byte(*req.Body)
	sig, err := parseSignature(req.Params.XEBAYSIGNATURE)
	if err != nil {
		return Forgotten{}, err
	}
	key, err := d.PublicKey(ctx, sig.Kid)
	if err != nil {
		// eBay answered no key, or could not be reached: either way eBay
		// should send the notice again, so this is not a 412.
		return Forgotten{}, app.Upstream(err)
	}
	if err := Verify(body, sig, key); err != nil {
		return Forgotten{}, err
	}

	var n notice
	if err := json.Unmarshal(body, &n); err != nil {
		return Forgotten{}, fmt.Errorf("%w: notice: %v", app.ErrBadInput, err)
	}
	// A topic this endpoint does not handle, or a notice naming nobody, is
	// acknowledged and ignored, so eBay does not keep sending it.
	if n.Metadata.Topic != Topic || n.Notification.Data.Username == "" {
		return Forgotten{}, nil
	}
	count, err := d.ForgetSeller(ctx, n.Notification.Data.Username)
	if err != nil {
		return Forgotten{}, err
	}
	// The notice id and the count, never the username: logging who closed
	// their account would keep the very thing the notice says to forget.
	slog.Info("ebay account deletion", "notification", n.Notification.NotificationID, "listings", count)
	return Forgotten{Listings: count}, nil
}

func (NotifyEndpoint) Build(f Forgotten) Forgotten { return f }

func (NotifyEndpoint) Render(Forgotten) apigen.EbayAccountDeletionResponseObject {
	return apigen.EbayAccountDeletion204Response{}
}

// -----------------------------------------------------------------------------
// Signature
// -----------------------------------------------------------------------------

// Signature is X-EBAY-SIGNATURE decoded: base64 of a JSON object naming the
// key (Kid) and carrying an ECDSA signature over the SHA-1 of the body.
type Signature struct {
	Alg       string `json:"alg"`
	Kid       string `json:"kid"`
	Signature string `json:"signature"`
	Digest    string `json:"digest"`
}

func parseSignature(header string) (Signature, error) {
	if header == "" {
		return Signature{}, fmt.Errorf("%w: no X-EBAY-SIGNATURE", ErrBadSignature)
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return Signature{}, fmt.Errorf("%w: X-EBAY-SIGNATURE is not base64", ErrBadSignature)
	}
	var s Signature
	if err := json.Unmarshal(raw, &s); err != nil {
		return Signature{}, fmt.Errorf("%w: X-EBAY-SIGNATURE is not JSON", ErrBadSignature)
	}
	if !strings.EqualFold(s.Alg, "ecdsa") || !strings.EqualFold(s.Digest, "sha1") || s.Kid == "" || s.Signature == "" {
		return Signature{}, fmt.Errorf("%w: unsupported signature (alg %q, digest %q)", ErrBadSignature, s.Alg, s.Digest)
	}
	return s, nil
}

// Verify checks sig over body with keyPEM, the key eBay serves for sig.Kid.
func Verify(body []byte, sig Signature, keyPEM string) error {
	key, err := parseKey(keyPEM)
	if err != nil {
		return err
	}
	der, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not base64", ErrBadSignature)
	}
	sum := sha1.Sum(body)
	if !ecdsa.VerifyASN1(key, sum[:], der) {
		return ErrBadSignature
	}
	return nil
}

// parseKey reads the key eBay serves, which may be PEM on one line, with
// no line breaks around or inside its base64.
func parseKey(keyPEM string) (*ecdsa.PublicKey, error) {
	b64 := strings.NewReplacer(
		"-----BEGIN PUBLIC KEY-----", "", "-----END PUBLIC KEY-----", "",
		"\n", "", "\r", "", " ", "", "\t", "",
	).Replace(keyPEM)
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, app.Upstream(fmt.Errorf("eBay public key is not base64: %w", err))
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, app.Upstream(fmt.Errorf("eBay public key: %w", err))
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, app.Upstream(fmt.Errorf("eBay public key is %T, not ECDSA", pub))
	}
	return key, nil
}
