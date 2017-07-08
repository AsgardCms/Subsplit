package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/go-redis/redis"
	log "gopkg.in/inconshreveable/log15.v2"
)

func startHTTPServer(client *redis.Client, cfg *config) {
	http.HandleFunc(cfg.HTTP.Route, func(w http.ResponseWriter, r *http.Request) {
		content, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.Write([]byte("Something went wrong !"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		client.LPush(fmt.Sprintf("%s:incoming", redisPreix), string(content))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Thanks!"))
	})

	log.Info(fmt.Sprintf("Starting HTTP server on port %d with route %s", cfg.HTTP.Port, cfg.HTTP.Route))
	log.Error(http.ListenAndServe(fmt.Sprintf(":%d", cfg.HTTP.Port), nil).Error())
	os.Exit(1)
}
