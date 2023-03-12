package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type GrentonInput interface {
	SetOn()
}

type InputServer struct {
	server http.Server
	gSet   *GrentonSet
}

func (is *InputServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Content-Type"), "multipart/form-data") {
		http.Error(w, "Unsupported Media Type, expected multipart/form-data", http.StatusUnsupportedMediaType)
		return
	}

	clu := r.FormValue("clu")
	id, convErr := strconv.Atoi(r.FormValue("id"))
	if convErr != nil {
		http.Error(w, "failed to convert string id to int", http.StatusBadRequest)
		return
	}

	foundInput := false
	var gInput GrentonInput

	for _, gSetClu := range is.gSet.Clus {
		if strings.EqualFold(gSetClu.Id, clu) {
			for _, gMotionSensor := range gSetClu.MotionSensors {
				if id == int(gMotionSensor.Id) {
					foundInput = true
					gInput = gMotionSensor
				}
			}
		}
	}

	if !foundInput {
		http.Error(w, "clu/id combination not found", http.StatusNotFound)
		return
	}

	gInput.SetOn()
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
