/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type config struct {
	listenAddr          string
	jevEndpoint         string
	jevTimeout          time.Duration
	jevAPIKey           string
	routingInstructions string
}

func loadConfigFrom(getenv func(string) string) (config, error) {
	cfg := config{
		listenAddr:          strings.TrimSpace(getenv("LISTEN_ADDR")),
		jevEndpoint:         strings.TrimSpace(getenv("JEV_ENDPOINT")),
		jevAPIKey:           strings.TrimSpace(getenv("JEV_API_KEY")),
		routingInstructions: strings.TrimSpace(getenv("ROUTING_INSTRUCTIONS")),
		jevTimeout:          10 * time.Second,
	}
	if cfg.listenAddr == "" {
		cfg.listenAddr = ":8080"
	}
	if cfg.routingInstructions == "" {
		cfg.routingInstructions = defaultRoutingInstructions
	}
	if cfg.jevEndpoint == "" {
		return config{}, errors.New("JEV_ENDPOINT is required")
	}
	parsed, err := url.Parse(cfg.jevEndpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return config{}, errors.New("JEV_ENDPOINT must be an absolute HTTP(S) URL without userinfo or fragment")
	}
	if raw := strings.TrimSpace(getenv("JEV_TIMEOUT")); raw != "" {
		cfg.jevTimeout, err = time.ParseDuration(raw)
		if err != nil || cfg.jevTimeout <= 0 {
			return config{}, errors.New("JEV_TIMEOUT must be a positive duration")
		}
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfigFrom(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	logger := log.New(os.Stdout, "jev-decision-service ", log.LstdFlags|log.LUTC)
	client := jevClient{
		endpoint:     cfg.jevEndpoint,
		apiKey:       cfg.jevAPIKey,
		instructions: cfg.routingInstructions,
		httpClient:   &http.Client{Timeout: cfg.jevTimeout},
	}
	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           newHandler(client, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.jevTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Printf("listening addr=%q", cfg.listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
