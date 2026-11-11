// Package multipartform deletes the temp files that multipart form parsing
// leaves behind.
//
// Request.ParseMultipartForm, and FormValue and FormFile which call it,
// spool file parts larger than maxMemory to os.TempDir() as multipart-*
// files. net/http deletes them only for the *http.Request it passed to the
// server's handler. chi's router, its Logger middleware and the auth
// middleware all hand handlers copies made with WithContext, so the server
// never sees a form parsed further down and its temp files stay on disk for
// good.
package multipartform

import (
	"log"
	"net/http"
)

// Release deletes the temp files of the multipart form parsed on r, if any.
//
// Defer it before parsing, so a form that parsed and was then rejected is
// released as well; ParseMultipartForm sets r.MultipartForm even when it
// reports a malformed URL query. The deferred call must run only once nothing
// reads the uploaded files, which holds as long as the handler copies them
// before it returns.
func Release(r *http.Request) {
	if r.MultipartForm == nil {
		return
	}
	if err := r.MultipartForm.RemoveAll(); err != nil {
		log.Printf("multipart: remove temp files: %v", err)
	}
}

// Cleanup releases the multipart form parsed on the request it passes to
// next, the way net/http does for the request it creates. It covers handlers
// that parse the request they are given, including implicitly through
// FormValue. A form parsed on a later copy, such as the one auth.RequireAuth
// makes, is not visible here; whoever parses that copy releases it.
func Cleanup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer Release(r)
		next.ServeHTTP(w, r)
	})
}
