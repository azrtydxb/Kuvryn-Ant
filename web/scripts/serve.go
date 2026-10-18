//go:build ignore

// Static-only production preview. Management API requests return 404;
// this intentionally does not simulate a backend or weaken authentication.
package main

import (
 "log"
 "net/http"
 "time"
 "github.com/azrtydxb/Kuvryn-Ant/internal/console"
)
func main() {
 server:=&http.Server{Addr:"127.0.0.1:5173",Handler:console.Handler(),ReadHeaderTimeout:5*time.Second}
 log.Fatal(server.ListenAndServe())
}
