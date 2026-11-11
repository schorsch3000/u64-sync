package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuth(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	get := func(h http.Handler, user, pass string, creds bool) int {
		r := httptest.NewRequest("GET", "/", nil)
		if creds {
			r.SetBasicAuth(user, pass)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	open := (&Server{}).auth(ok)
	if get(open, "", "", false) != 204 {
		t.Fatal("no password configured must not require auth")
	}
	h := (&Server{User: "admin", Password: "s3cret"}).auth(ok)
	for _, c := range []struct {
		u, p  string
		creds bool
		want  int
	}{{"", "", false, 401}, {"admin", "wrong", true, 401}, {"other", "s3cret", true, 401}, {"admin", "s3cret", true, 204}} {
		if got := get(h, c.u, c.p, c.creds); got != c.want {
			t.Errorf("%+v: got %d want %d", c, got, c.want)
		}
	}
}
