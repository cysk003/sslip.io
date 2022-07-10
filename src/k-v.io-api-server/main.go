package main

import (
	"fmt"
	"log"
	"net/http"
)

type etcdHandler struct {
	n int
}

func (h *etcdHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.n++
	_, _ = fmt.Fprintf(w, "count is %d\n", h.n)
}

func main() {
	http.Handle("/v0/kv", new(etcdHandler))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
