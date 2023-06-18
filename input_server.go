package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
)

type GrentonInput interface {
	Set(bool)
}

type InputServer struct {
	server http.Server
	gSet   *GrentonSet
}

func (is *InputServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
	is.gSet.Debugf("input server handling request from host: %s\n", r.Host)

	if !strings.EqualFold(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "unsupported Media Type, expected application/json", http.StatusUnsupportedMediaType)
		return
	}

	type input struct {
		Clu   string
		Id    string
		State bool
	}

	inputPayload := &input{}

	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(inputPayload)
	if err != nil {
		is.gSet.Error(errors.Wrapf(err, "failed to decode request body from host: %s", r.Host))
		return
	}

	sensor, err := is.gSet.FindMotionSensor(inputPayload.Clu, inputPayload.Id)
	if err != nil {
		is.gSet.Error(err)
		return
	}

	sensor.Set(inputPayload.State)

	w.WriteHeader(http.StatusOK)
}

func (is *InputServer) Run() error {
	return is.server.ListenAndServe()
}

func NewInputServer(grentonSet *GrentonSet, port int) *InputServer {
	is := &InputServer{
		gSet: grentonSet,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/update", is.HandleRequest)

	is.server = http.Server{
		Addr:           fmt.Sprintf(":%d", port),
		Handler:        mux,
		ReadTimeout:    4 * time.Second,
		WriteTimeout:   4 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	return is
}
