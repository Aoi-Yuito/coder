package trialer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/coder/coderd/database"
	"github.com/coder/coder/enterprise/coderd/license"
	"github.com/google/uuid"
)

type request struct {
	DeploymentID string `json:"deployment_id"`
	Email        string `json:"email"`
}

// New creates a handler that can issue trial licenses!
func New(db database.Store, url string, keys map[string]ed25519.PublicKey) func(ctx context.Context, email string) error {
	return func(ctx context.Context, email string) error {
		deploymentID, err := db.GetDeploymentID(ctx)
		if err != nil {
			return xerrors.Errorf("get deployment id: %w", err)
		}
		data, err := json.Marshal(request{
			DeploymentID: deploymentID,
			Email:        email,
		})
		if err != nil {
			return xerrors.Errorf("marshal: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return xerrors.Errorf("create license request: %w", err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return xerrors.Errorf("perform license request: %w", err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			return xerrors.Errorf("read license: %w", err)
		}
		rawClaims, err := license.ParseRaw(string(raw), keys)
		if err != nil {
			return xerrors.Errorf("parse license: %w", err)
		}
		exp, ok := rawClaims["exp"].(float64)
		if !ok {
			return xerrors.New("invalid license missing exp claim")
		}
		expTime := time.Unix(int64(exp), 0)

		claims, err := license.ParseClaims(string(raw), keys)
		if err != nil {
			return xerrors.Errorf("parse claims: %w", err)
		}
		id, err := uuid.Parse(claims.ID)
		_, err = db.InsertLicense(ctx, database.InsertLicenseParams{
			UploadedAt: database.Now(),
			JWT:        string(raw),
			Exp:        expTime,
			Uuid: uuid.NullUUID{
				UUID:  id,
				Valid: err == nil,
			},
		})
		if err != nil {
			return xerrors.Errorf("insert license: %w", err)
		}
		return nil
	}
}
