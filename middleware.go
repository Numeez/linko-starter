package main

import (
	"crypto/rand"
	"net/http"
)

func requestIdMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestId := r.Header.Get("X-Request-ID")
		if requestId == "" {
			requestId = rand.Text()
		}
		w.Header().Set("X-Request-ID", requestId)
		next.ServeHTTP(w, r)
	})
}
