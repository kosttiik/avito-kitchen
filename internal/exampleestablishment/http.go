package exampleestablishment

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
)

type MenuItemUpdate struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	PriceMinor  *int64  `json:"price_minor,omitempty"`
	Available   *bool   `json:"available,omitempty"`
}

type decisionRequest struct {
	Decision string  `json:"decision"`
	Reason   *string `json:"reason,omitempty"`
}

func NewHandler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, contract.HealthResponse{Status: "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !service.Ready() {
			writeError(w, http.StatusServiceUnavailable, errors.New("initial menu sync has not completed"))
			return
		}
		writeJSON(w, http.StatusOK, contract.HealthResponse{Status: "ok"})
	})
	mux.HandleFunc("GET /operator/v1/menu-items", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": service.Menu()})
	})
	mux.HandleFunc("PATCH /operator/v1/menu-items/{externalId}", func(w http.ResponseWriter, r *http.Request) {
		var update MenuItemUpdate
		if err := decodeJSON(w, r, &update); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if update.Name == nil && update.Description == nil && update.PriceMinor == nil && update.Available == nil {
			writeError(w, http.StatusBadRequest, errors.New("at least one field is required"))
			return
		}
		item, err := service.UpdateMenuItem(r.Context(), r.PathValue("externalId"), update)
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			} else if strings.Contains(err.Error(), "invalid") {
				status = http.StatusBadRequest
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	})
	mux.HandleFunc("GET /operator/v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": service.Orders()})
	})
	mux.HandleFunc("POST /operator/v1/orders/{orderId}/decision", func(w http.ResponseWriter, r *http.Request) {
		var request decisionRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		order, err := service.Decide(r.Context(), r.PathValue("orderId"), request.Decision, request.Reason)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, order)
	})
	mux.HandleFunc("PATCH /operator/v1/orders/{orderId}/status", func(w http.ResponseWriter, r *http.Request) {
		var request contract.OrderStatusRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		order, err := service.UpdateStatus(r.Context(), r.PathValue("orderId"), string(request.Status))
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, order)
	})
	return mux
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid request body")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": err.Error()}})
}
