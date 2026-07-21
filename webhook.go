package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/domains"
	"github.com/plaid/plaid-go/v43/plaid"
)

// webhookPath extracts the route path from the configured webhook URL so the
// server listens wherever Plaid was told to deliver.
func webhookPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return "/hook/plaid"
	}
	return u.Path
}

func handlePlaidWebhook(plaidClient *plaid.APIClient, store *database.Store, cfg *Config, tg *TelegramBot) http.HandlerFunc {
	verifier := newPlaidWebhookVerifier(plaidClient)
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		token := r.Header.Get("Plaid-Verification")
		if token == "" {
			slog.Warn("webhook missing verification header", "for", ip)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := verifier.verify(r.Context(), token, body); err != nil {
			slog.Warn("rejected plaid webhook", "for", ip, "err", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var hook struct {
			WebhookType string `json:"webhook_type"`
			WebhookCode string `json:"webhook_code"`
			ItemID      string `json:"item_id"`
		}
		if err := json.Unmarshal(body, &hook); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		slog.Info("plaid webhook", "type", hook.WebhookType, "code", hook.WebhookCode, "item", hook.ItemID)

		// Anything other than a sync signal is acknowledged and ignored;
		// non-2xx would just make Plaid retry a webhook we don't act on.
		if hook.WebhookType != "TRANSACTIONS" || hook.WebhookCode != "SYNC_UPDATES_AVAILABLE" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if strings.HasPrefix(hook.ItemID, "seed:") {
			w.WriteHeader(http.StatusOK)
			return
		}
		item, err := store.GetItemByID(r.Context(), hook.ItemID)
		if err != nil {
			slog.Error("webhook for unknown item", "item", hook.ItemID, "err", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		accessToken, err := DecryptColumnSecret(item.AccessToken, item.ItemID, cfg.DBCryptKey)
		if err != nil {
			slog.Error("failed to decrypt access token", "item", item.ItemID, "err", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Ack immediately and sync in the background; syncCtx outlives the
		// request, and inFlightSyncs dedupes bursts of webhooks per item.
		usql, err := store.GetUserByID(r.Context(), item.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			// Permanent state — a non-2xx would just make Plaid retry a
			// webhook we can never act on.
			slog.Info("user not located in database", "ID", item.UserID)
			w.WriteHeader(http.StatusOK)
			return
		} else if err != nil {
			slog.Error("failed to query database for user", "ID", item.UserID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		u := domains.NewUser(usql)
		syncCtx := context.WithoutCancel(r.Context())
		go func() {
			if err := syncItem(syncCtx, item.ItemID, accessToken, item.Cursor, plaidClient, store, cfg); err != nil {
				slog.Error("webhook-triggered sync failed", "item", item.ItemID, "err", err)
			}
			// Kick even after a failed sync: earlier pages may have committed.
			if u != nil {
				tg.startDrain(u.TelegramID())
			}
		}()
		w.WriteHeader(http.StatusOK)
	}
}

// plaidWebhookVerifier checks the Plaid-Verification header: an ES256 JWT
// whose claims carry a SHA-256 of the request body. Verification keys are
// fetched from Plaid by key id and cached, per Plaid's guidance.
type plaidWebhookVerifier struct {
	client *plaid.APIClient
	mu     sync.Mutex
	keys   map[string]*ecdsa.PublicKey
}

func newPlaidWebhookVerifier(client *plaid.APIClient) *plaidWebhookVerifier {
	return &plaidWebhookVerifier{client: client, keys: make(map[string]*ecdsa.PublicKey)}
}

func (v *plaidWebhookVerifier) verify(ctx context.Context, token string, body []byte) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("malformed verification jwt")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode jwt header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return fmt.Errorf("parse jwt header: %w", err)
	}
	if header.Alg != "ES256" {
		return fmt.Errorf("unexpected jwt alg %q", header.Alg)
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode jwt signature: %w", err)
	}
	if len(sig) != 64 {
		return errors.New("malformed ES256 signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sigR := new(big.Int).SetBytes(sig[:32])
	sigS := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(key, digest[:], sigR, sigS) {
		return errors.New("invalid jwt signature")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode jwt claims: %w", err)
	}
	var claims struct {
		Iat               int64  `json:"iat"`
		RequestBodySha256 string `json:"request_body_sha256"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return fmt.Errorf("parse jwt claims: %w", err)
	}
	if time.Since(time.Unix(claims.Iat, 0)) > 5*time.Minute {
		return errors.New("verification jwt is stale")
	}
	bodyDigest := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(bodyDigest[:])), []byte(claims.RequestBodySha256)) != 1 {
		return errors.New("request body digest mismatch")
	}
	return nil
}

func (v *plaidWebhookVerifier) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	v.mu.Lock()
	cached, ok := v.keys[kid]
	v.mu.Unlock()
	if ok {
		return cached, nil
	}

	req := plaid.NewWebhookVerificationKeyGetRequest(kid)
	resp, _, err := v.client.PlaidApi.WebhookVerificationKeyGet(ctx).WebhookVerificationKeyGetRequest(*req).Execute()
	if err != nil {
		return nil, fmt.Errorf("fetch webhook verification key: %w", err)
	}
	jwk := resp.GetKey()
	if jwk.GetKty() != "EC" || jwk.GetCrv() != "P-256" {
		return nil, fmt.Errorf("unexpected verification key type %s/%s", jwk.GetKty(), jwk.GetCrv())
	}
	xb, err := base64.RawURLEncoding.DecodeString(jwk.GetX())
	if err != nil {
		return nil, fmt.Errorf("decode jwk x: %w", err)
	}
	yb, err := base64.RawURLEncoding.DecodeString(jwk.GetY())
	if err != nil {
		return nil, fmt.Errorf("decode jwk y: %w", err)
	}
	key := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xb),
		Y:     new(big.Int).SetBytes(yb),
	}

	v.mu.Lock()
	v.keys[kid] = key
	v.mu.Unlock()
	return key, nil
}
