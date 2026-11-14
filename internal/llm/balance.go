package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go-monolith/metrics"
)

// BalanceInterval is how often the balance of the paid account is read.
const BalanceInterval = time.Hour

// fetchBalance reads the balance endpoint of the provider: currency -> money left.
func fetchBalance(ctx context.Context, client *http.Client, baseURL, apiKey string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/user/balance", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// The body is left out: an error page could echo what was sent.
		return nil, fmt.Errorf("the balance endpoint answered %s", resp.Status)
	}
	var body struct {
		Infos []struct {
			Currency string `json:"currency"`
			Total    string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return nil, fmt.Errorf("the balance reply cannot be read: %w", err)
	}
	out := make(map[string]float64, len(body.Infos))
	for _, info := range body.Infos {
		total, err := strconv.ParseFloat(info.Total, 64)
		if err != nil {
			return nil, fmt.Errorf("the balance reply has a bad number for %s", info.Currency)
		}
		out[info.Currency] = total
	}
	return out, nil
}

// readBalance reads the balance once and sets the gauge. It reports whether it worked.
func readBalance(ctx context.Context, client *http.Client, baseURL, apiKey string, logger *slog.Logger) bool {
	balances, err := fetchBalance(ctx, client, baseURL, apiKey)
	if err != nil {
		metrics.PaidLLMRequests.WithLabelValues(deepSeekProvider, "balance_error").Inc()
		logger.Warn("could not read the balance of the paid LLM account", slog.Any("error", err))
		return false
	}
	for currency, total := range balances {
		metrics.PaidLLMBalance.WithLabelValues(currency).Set(total)
	}
	return true
}

// WatchBalance sets paid_llm_balance now and then every interval until ctx ends, so an
// alert can say the account is nearly empty before the provider starts refusing requests
// (402). It sends only the key, to the balance endpoint.
func WatchBalance(ctx context.Context, apiKey string, interval time.Duration, logger *slog.Logger) {
	client := &http.Client{Timeout: 20 * time.Second}
	readBalance(ctx, client, DeepSeekBaseURL, apiKey, logger)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			readBalance(ctx, client, DeepSeekBaseURL, apiKey, logger)
		}
	}
}
