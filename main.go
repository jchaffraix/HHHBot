package main

import (
  "log"
  "net/http"
  "os"
)

func logRequest(req *http.Request) {
  log.Printf("Received request for %s", req.URL.String())
}

func mainPageHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  if req.URL.Path != "/" {
      http.NotFound(w, req)
      return
  }

  // TODO: Check for login credentials.

  http.ServeFile(w, req, "index.html")
}

func main() {
  http.HandleFunc("/", mainPageHandler)

  port := os.Getenv("PORT")
  if port == "" {
    port = "8080"
  }

  log.Printf("Listening on port=%s", port)
  if err := http.ListenAndServe(":" + port, nil); err != nil {
    log.Fatal(err)
  }
}
