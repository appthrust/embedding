package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func warmup(model string) func(context.Context, *http.Client, string) error {
	body, _ := json.Marshal(map[string]string{"model": model, "input": "warm up"})
	return func(ctx context.Context, client *http.Client, upstream string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("embedding warm-up returned HTTP %d", resp.StatusCode)
		}
		var result struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			return err
		}
		if len(result.Data) != 1 || len(result.Data[0].Embedding) != embeddingDimensions {
			return errors.New("embedding warm-up must return one 768-dimensional vector")
		}
		return nil
	}
}
