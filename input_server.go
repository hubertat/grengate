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

// HandleRequest applies a motion sensor push: POST /update with {Clu, Id, State}.
// A body that is not such an object is answered with 400, an id that is not a configured
// motion sensor with 404.
func (is *InputServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
	is.gSet.Debugf("input server handling request from host: %s\n", r.Host)

	if !strings.EqualFold(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "unsupported Media Type, expected application/json", http.StatusUnsupportedMediaType)
		return
	}

	var in struct {
		Clu   string
		Id    string
		State *bool
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		is.gSet.Error(errors.Wrapf(err, "input server: bad request body from host: %s", r.Host))
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if in.Clu == "" || in.Id == "" || in.State == nil {
		is.gSet.Error(fmt.Errorf("input server: request from host %s lacks Clu, Id or State", r.Host))
		http.Error(w, "Clu, Id and State are required", http.StatusBadRequest)
		return
	}

	d, _ := is.gSet.registry.find(in.Clu, in.Id)
	sensor, ok := d.(*MotionSensor)
	if !ok {
		is.gSet.Error(fmt.Errorf("input server: no motion sensor %s|%s configured", in.Clu, in.Id))
		http.Error(w, "unknown motion sensor", http.StatusNotFound)
		return
	}

	sensor.Set(*in.State)
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
