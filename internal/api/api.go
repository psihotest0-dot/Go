package api

import (
	"encoding/json"
	"net/http"

	"go-monolith/internal/db"
)

type API struct {
	db *db.Database
}

func New(db *db.Database) *API {
	return &API{db: db}
}

func (a *API) HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "OK"})
}
