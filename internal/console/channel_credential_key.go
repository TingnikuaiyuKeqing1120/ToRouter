package console

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

func channelCredentialKeyRoutes(r chi.Router, s *store.Store) {
	r.Get("/channels/{channelID}/credentials/{credentialID}/key", func(w http.ResponseWriter, r *http.Request) {
		id, credentialID, ok := credentialOperationTarget(w, r, true)
		if !ok {
			return
		}
		channel, err := s.Channel(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fail(w, http.StatusNotFound, "channel or API key credential not found")
			} else {
				fail(w, http.StatusInternalServerError, "cannot read channel credential")
			}
			return
		}
		if channel.CredentialType == store.CredentialAPIKey {
			for _, credential := range channel.Credentials {
				if credential.ID == credentialID && credential.OAuth == nil && credential.APIKey != "" {
					respond(w, http.StatusOK, map[string]string{"key": credential.APIKey})
					return
				}
			}
		}
		fail(w, http.StatusNotFound, "channel or API key credential not found")
	})
}
