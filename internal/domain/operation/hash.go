package operation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
)

// PayloadHash returns the SHA-256 hexadecimal digest of the canonical business payload.
// The JSON uses lexicographically ordered keys, no spaces, literal UTF-8, and only escapes required by JSON.
func PayloadHash(request Request) (string, error) {
	if err := validateInput(request); err != nil {
		return "", err
	}
	payload := canonicalPayload{
		ExternalTransactionID: request.ExternalTransactionID,
		GameID:                request.GameID,
		Kind:                  request.Kind,
		Money:                 request.Money,
		PlayerID:              request.PlayerID,
		ProviderID:            request.ProviderID,
		RoundID:               request.RoundID,
		WalletID:              request.WalletID,
	}
	if request.ReferenceExternalTransactionID != nil {
		payload.ReferenceExternalTransactionID = request.ReferenceExternalTransactionID
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return "", err
	}
	canonicalJSON := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	digest := sha256.Sum256(canonicalJSON)
	return hex.EncodeToString(digest[:]), nil
}

type canonicalPayload struct {
	ExternalTransactionID          string           `json:"externalTransactionId"`
	GameID                         string           `json:"gameId"`
	Kind                           wallet.WagerKind `json:"kind"`
	Money                          money.Money      `json:"money"`
	PlayerID                       string           `json:"playerId"`
	ProviderID                     string           `json:"providerId"`
	ReferenceExternalTransactionID *string          `json:"referenceExternalTransactionId,omitempty"`
	RoundID                        string           `json:"roundId"`
	WalletID                       string           `json:"walletId"`
}
