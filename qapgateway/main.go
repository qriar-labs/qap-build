package main

import (
  "bytes"
  "context"
  "encoding/json"
  "fmt"
  "io"
  "net/http"
  "os"
  "strconv"
  "strings"
  "sync"
  "time"

  "github.com/nats-io/nats.go"
)

/* =========================
   Config / Env
========================= */

type Config struct {
  NatsUrl            string
  NatsToken          string
  NatsQueueQAP       string
  NatsQueueAnalytics string
  NatsQueueSecurity  string
  EndpointCheck      string

  PublishFromServer  bool
  PublishFromClient  bool
  MaxBodyCapture     int64
}

func GetConfig() *Config {
  return &Config{
    NatsUrl:            os.Getenv("NATS_URL"),
    NatsToken:          os.Getenv("NATS_TOKEN"),
    NatsQueueQAP:       os.Getenv("NATS_QUEUE_QAP"),
    NatsQueueAnalytics: os.Getenv("NATS_QUEUE_ANALYTICS"),
    NatsQueueSecurity:  os.Getenv("NATS_QUEUE_SECURITY"),
    EndpointCheck:      os.Getenv("ENDPOINT_CHECK"),

    PublishFromServer:  getBoolEnv("PUBLISH_FROM_SERVER", true),
    PublishFromClient:  getBoolEnv("PUBLISH_FROM_CLIENT", true),
    MaxBodyCapture:     getInt64Env("MAX_BODY_CAPTURE", 1<<20), // 1MB
  }
}

func getBoolEnv(key string, def bool) bool {
  v := strings.TrimSpace(os.Getenv(key))
  if v == "" {
    return def
  }
  switch strings.ToLower(v) {
  case "1", "true", "t", "yes", "y":
    return true
  case "0", "false", "f", "no", "n":
    return false
  default:
    return def
  }
}

func getInt64Env(key string, def int64) int64 {
  v := strings.TrimSpace(os.Getenv(key))
  if v == "" {
    return def
  }
  n, err := strconv.ParseInt(v, 10, 64)
  if err != nil || n <= 0 {
    return def
  }
  return n
}

/* =========================
   NATS
========================= */

type Nats struct {
  Conn               *nats.Conn
  NatsUrl            string
  NatsToken          string
  NatsQueueQAP       string
  NatsQueueAnalytics string
  NatsQueueSecurity  string
}

func NewNats(natsUrl, natsToken, natsQueueQAP, natsQueueAnalytics, natsQueueSecurity string) *Nats {
  return &Nats{
    NatsUrl:            natsUrl,
    NatsToken:          natsToken,
    NatsQueueQAP:       natsQueueQAP,
    NatsQueueAnalytics: natsQueueAnalytics,
    NatsQueueSecurity:  natsQueueSecurity,
  }
}

func (n *Nats) StartConn() {
  conn, err := nats.Connect(
    n.NatsUrl,
    nats.Token(n.NatsToken),
    nats.MaxReconnects(-1),
    nats.ReconnectWait(10*time.Second),
    nats.DisconnectHandler(func(_ *nats.Conn) {
      logger.Error("Disconnected from NATS")
    }),
    nats.ReconnectHandler(func(_ *nats.Conn) {
      logger.Debug("Reconnected to NATS")
    }),
    nats.ClosedHandler(func(_ *nats.Conn) {
      logger.Error("Connection to NATS closed")
    }),
  )
  if err != nil {
    logger.Error("Failed to connect to NATS:", err)
    return
  }
  n.Conn = conn
}

func (n *Nats) publish(subject string, message []byte) {
  if subject == "" {
    return
  }
  if n.Conn == nil || !n.Conn.IsConnected() {
    logger.Error("Cannot publish: NATS connection not established")
    return
  }
  if err := n.Conn.Publish(subject, message); err != nil {
    logger.Error("Failed to publish message:", err)
  }
}

func (n *Nats) PublishQAP(message []byte)       { n.publish(n.NatsQueueQAP, message) }
func (n *Nats) PublishAnalytics(message []byte) { n.publish(n.NatsQueueAnalytics, message) }
func (n *Nats) PublishSecurity(message []byte)  { n.publish(n.NatsQueueSecurity, message) }

/* =========================
   Tipos de mensagens
========================= */

type RequestObject struct {
  Source     string              `json:"source"`   // "router" | "client"
  Type       string              `json:"type"`     // "request"
  Path       string              `json:"path"`
  Method     string              `json:"method"`
  HttpStatus int                 `json:"http_status"`
  Headers    map[string][]string `json:"headers"`
  Body       string              `json:"body"`
}

type ResponseObject struct {
  Source     string              `json:"source"`   // "router" | "client"
  Type       string              `json:"type"`     // "response"
  Path       string              `json:"path"`
  Method     string              `json:"method"`
  HttpStatus int                 `json:"http_status"`
  Headers    map[string][]string `json:"headers"`
  Body       string              `json:"body"`
}

/* =========================
   Utilidades
========================= */

var endpointMap = map[string]string{
  "/csscolornames/colors": "/css/cores",
}

func copyHeaders(h http.Header) map[string][]string {
  out := make(map[string][]string, len(h))
  for k, v := range h {
    cp := make([]string, len(v))
    copy(cp, v)
    out[k] = cp
  }
  return out
}

// lê e trunca corpo até maxBytes
func readAndTruncateBody(rc io.ReadCloser, maxBytes int64) ([]byte, error) {
  if rc == nil {
    return nil, nil
  }
  defer rc.Close()
  var buf bytes.Buffer
  _, err := io.CopyN(&buf, rc, maxBytes+1)
  if err != nil && err != io.EOF {
    return nil, err
  }
  b := buf.Bytes()
  if int64(len(b)) > maxBytes {
    b = b[:maxBytes]
  }
  return b, nil
}

func truncateString(s string, max int64) string {
  if int64(len(s)) <= max {
    return s
  }
  return s[:max]
}

func AccessCheck(endpoint string, header http.Header) (bool, error) {
  req, err := http.NewRequest("GET", endpoint, nil)
  if err != nil {
    return false, err
  }
  req.Header = header.Clone()
  resp, err := http.DefaultClient.Do(req)
  if err != nil {
    return false, err
  }
  defer resp.Body.Close()

  if resp.StatusCode != http.StatusOK {
    body, _ := io.ReadAll(resp.Body)
    return false, fmt.Errorf("❌ endpoint: %v - return: %v - %v", endpoint, resp.StatusCode, string(body))
  }
  return true, nil
}

/* =========================
   Token store
========================= */

type TokenStore struct {
  AccessToken  string
  RefreshToken string
  ExpiresAt    time.Time
  mu           sync.Mutex
}

var tokenStore = &TokenStore{}

func getSystemAccessToken() (string, error) {
  tokenStore.mu.Lock()
  defer tokenStore.mu.Unlock()

  if time.Now().Before(tokenStore.ExpiresAt.Add(-10 * time.Second)) && tokenStore.AccessToken != "" {
    return tokenStore.AccessToken, nil
  }

  // tenta refresh
  if tokenStore.RefreshToken != "" {
    if token, refresh, exp, err := requestToken("refresh_token", tokenStore.RefreshToken); err == nil {
      tokenStore.AccessToken = token
      tokenStore.RefreshToken = refresh
      tokenStore.ExpiresAt = exp
      return token, nil
    }
  }

  // fluxo inicial conforme template
  token, refresh, exp, err := requestToken("password", "")
  if err != nil {
    return "", err
  }
  tokenStore.AccessToken = token
  tokenStore.RefreshToken = refresh
  tokenStore.ExpiresAt = exp
  return token, nil
}

func requestToken(grantType string, refreshToken string) (string, string, time.Time, error) {
  var form string
  switch grantType {
  case "password":
    form = fmt.Sprintf("grant_type=password&client_id=qap-front&client_secret=MzAwHubJyLB6xwKFdQqZ7wswrmaCQGBz&username=qap-front&password=MzAwHubJyLB6xwKFdQqZ7wswrmaCQGBz")
  case "client_credentials":
    form = fmt.Sprintf("grant_type=client_credentials&client_id=qap-front&client_secret=MzAwHubJyLB6xwKFdQqZ7wswrmaCQGBz")
  case "refresh_token":
    fallthrough
  default:
    form = fmt.Sprintf("grant_type=refresh_token&client_id=qap-front&client_secret=MzAwHubJyLB6xwKFdQqZ7wswrmaCQGBz&refresh_token=%s", refreshToken)
  }

  req, err := http.NewRequest("POST", "https://keycloak-qa.valecard.com.br/realms/poc-qap/protocol/openid-connect/certs", strings.NewReader(form))
  if err != nil {
    return "", "", time.Time{}, err
  }
  req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

  resp, err := http.DefaultClient.Do(req)
  if err != nil {
    return "", "", time.Time{}, err
  }
  defer resp.Body.Close()

  if resp.StatusCode != http.StatusOK {
    body, _ := io.ReadAll(resp.Body)
    return "", "", time.Time{}, fmt.Errorf("❌ token request failed: %d - %s", resp.StatusCode, string(body))
  }

  var data struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token"`
    ExpiresIn    int    `json:"expires_in"`
  }
  if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
    return "", "", time.Time{}, err
  }

  expiry := time.Now().Add(time.Duration(data.ExpiresIn) * time.Second)
  return data.AccessToken, data.RefreshToken, expiry, nil
}

/* =========================
   Logger (Krakend)
========================= */

var logger Logger = nil

type Logger interface {
  Debug(v ...interface{})
  Info(v ...interface{})
  Warning(v ...interface{})
  Error(v ...interface{})
  Critical(v ...interface{})
  Fatal(v ...interface{})
}

/* =========================
   Client Plugin (http-client)
   -> Publica SOMENTE 2xx em QAP
========================= */

var pluginName = "qap-krakend-plugin"
var ClientRegisterer = registerer(pluginName)

type registerer string

func (r registerer) RegisterClients(f func(
  name string,
  handler func(context.Context, map[string]interface{}) (http.Handler, error),
)) {
  f(string(r), r.registerClients)
}

func (r registerer) registerClients(_ context.Context, extra map[string]interface{}) (http.Handler, error) {
  config := GetConfig()
  nats := NewNats(config.NatsUrl, config.NatsToken, config.NatsQueueQAP, config.NatsQueueAnalytics, config.NatsQueueSecurity)
  nats.StartConn()

  gatewayName := "unknown-gateway"
  if val, ok := extra["gateway_name"].(string); ok && val != "" {
    gatewayName = val
  }

  return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
    // Caminho Krakend "lógico"
    krakendPath := req.URL.Path
    if ep, ok := extra["endpoint"].(string); ok && ep != "" {
      krakendPath = ep
    }
    if val, ok := endpointMap[krakendPath]; ok {
      krakendPath = val
    }

    // Corpo original (truncado) e restaura
    originalBody, err := readAndTruncateBody(req.Body, config.MaxBodyCapture)
    if err != nil {
      logger.Error("Failed to read request body:", err)
      http.Error(w, "Invalid request body", http.StatusBadRequest)
      return
    }
    req.Body = io.NopCloser(bytes.NewBuffer(originalBody))

    // Access check (opcional)
    if strings.TrimSpace(config.EndpointCheck) != "" {
      valid, err := AccessCheck(config.EndpointCheck, req.Header)
      if err != nil {
        logger.Error("Checking service unavailable:", err)
        http.Error(w, "Checking service unavailable", http.StatusInternalServerError)
        return
      }
      if !valid {
        http.Error(w, "Access Denied", http.StatusForbidden)
        return
      }
    }

    // Forward: preserva token do usuário em X-Authorization-User
    forwardReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(originalBody))
    if err != nil {
      http.Error(w, "Failed to create forward request", http.StatusInternalServerError)
      return
    }
    forwardReq.Header = req.Header.Clone()

    userToken := req.Header.Get("X-User-Token")
    if userToken != "" {
      forwardReq.Header.Set("X-Authorization-User", userToken)
      logger.Debug("🔐 Preserving user token in X-Authorization-User")
    }

    // Token de sistema
    systemToken, err := getSystemAccessToken()
    if err != nil {
      logger.Error("🔐 Failed to get internal system token:", err)
      http.Error(w, "Internal authentication failed", http.StatusUnauthorized)
      return
    }
    forwardReq.Header.Set("Authorization", "Bearer "+systemToken)
    logger.Debug("🔐 Injected system token into Authorization header")

    // Chamada ao backend
    resp, err := http.DefaultClient.Do(forwardReq)
    if err != nil {
      http.Error(w, "Request failed: "+err.Error(), http.StatusInternalServerError)
      return
    }
    defer resp.Body.Close()

    respBody, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxBodyCapture))
    if err != nil {
      http.Error(w, "Failed to read response", http.StatusInternalServerError)
      return
    }

    userEmail := req.Header.Get("X-User-Email")

    // headers para evento
    mergedHeaders := copyHeaders(resp.Header)
    mergedHeaders["X-API-Path"] = []string{krakendPath}
    mergedHeaders["X-Status"] = []string{fmt.Sprintf("%d", resp.StatusCode)}
    mergedHeaders["X-Api-Key"] = []string{gatewayName}
    if userEmail != "" {
      mergedHeaders["X-User-Email"] = []string{userEmail}
    }
    if userToken != "" {
      mergedHeaders["X-Authorization-User"] = []string{userToken}
    }

    // ✅ Publica APENAS responses 2xx em QAP
    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
      respObj := ResponseObject{
        Source:     "client",
        Type:       "response",
        Path:       req.URL.Path,
        Method:     req.Method,
        HttpStatus: resp.StatusCode,
        Headers:    mergedHeaders,
        Body:       string(respBody),
      }
      if msg, err := json.Marshal(respObj); err == nil {
        nats.PublishQAP(msg)
      } else {
        logger.Error("Failed to marshal client response:", err)
      }
    }

    // Resposta ao cliente
    for k, hs := range resp.Header {
      for _, h := range hs {
        w.Header().Add(k, h)
      }
    }
    w.Header().Set("X-API-Path", krakendPath)
    w.Header().Set("X-Status", fmt.Sprintf("%d", resp.StatusCode))
    w.Header().Set("X-Api-Key", gatewayName)
    if userEmail != "" {
      w.Header().Set("X-User-Email", userEmail)
    }
    if userToken != "" {
      w.Header().Set("X-Authorization-User", userToken)
    }
    w.WriteHeader(resp.StatusCode)
    w.Write(respBody)
  }), nil
}

/* =========================
   Server Plugin (http-server/router)
   -> Publica em Analytics (gateway-level)
========================= */

var HandlerRegisterer = serverRegisterer(pluginName)

type serverRegisterer string

func (r serverRegisterer) RegisterHandlers(f func(
  name string,
  handler func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error),
)) {
  f(string(r), r.registerHandlers)
}

type rwCapture struct {
  http.ResponseWriter
  status int
  buf    bytes.Buffer
}

func (w *rwCapture) WriteHeader(code int) {
  w.status = code
  w.ResponseWriter.WriteHeader(code)
}

func (w *rwCapture) Write(b []byte) (int, error) {
  w.buf.Write(b) // cópia do corpo enviado ao cliente
  return w.ResponseWriter.Write(b)
}

func (r serverRegisterer) registerHandlers(_ context.Context, extra map[string]interface{}, next http.Handler) (http.Handler, error) {
  cfg := GetConfig()
  nc := NewNats(cfg.NatsUrl, cfg.NatsToken, cfg.NatsQueueQAP, cfg.NatsQueueAnalytics, cfg.NatsQueueSecurity)
  nc.StartConn()

  return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
    if cfg.PublishFromServer {
      // Request body (truncado) + restaura
      var reqBody []byte
      if req.Body != nil {
        b, _ := readAndTruncateBody(req.Body, cfg.MaxBodyCapture)
        reqBody = b
        req.Body = io.NopCloser(bytes.NewBuffer(b))
      }
      // publish REQUEST (router) -> Analytics only
      reqObj := RequestObject{
        Source:     "router",
        Type:       "request",
        Path:       req.URL.Path,
        Method:     req.Method,
        HttpStatus: 0,
        Headers:    req.Header,
        Body:       string(reqBody),
      }
      if msg, err := json.Marshal(reqObj); err == nil {
        nc.PublishAnalytics(msg)
      } else {
        logger.Error("ServerPlugin: marshal request:", err)
      }
    }

    // Continua cadeia do gateway
    rec := &rwCapture{ResponseWriter: w, status: 0}
    next.ServeHTTP(rec, req)
    if rec.status == 0 {
      rec.status = http.StatusOK
    }

    if cfg.PublishFromServer {
      respHeaders := copyHeaders(rec.Header())
      respHeaders["X-API-Path"] = []string{req.URL.Path}
      respHeaders["X-Status"] = []string{fmt.Sprintf("%d", rec.status)}

      respObj := ResponseObject{
        Source:     "router",
        Type:       "response",
        Path:       req.URL.Path,
        Method:     req.Method,
        HttpStatus: rec.status,
        Headers:    respHeaders,
        Body:       truncateString(rec.buf.String(), cfg.MaxBodyCapture),
      }
      if msg, err := json.Marshal(respObj); err == nil {
        nc.PublishAnalytics(msg)
      } else {
        logger.Error("ServerPlugin: marshal response:", err)
      }

      // (Opcional) Exemplos de publicação em Security (401/403)
      // if rec.status == http.StatusUnauthorized || rec.status == http.StatusForbidden {
      //   if msg, err := json.Marshal(respObj); err == nil {
      //     nc.PublishSecurity(msg)
      //   }
      // }
    }
  }), nil
}

/* =========================
   Krakend Logger registrar
========================= */

func (registerer) RegisterLogger(v interface{}) {
  if l, ok := v.(Logger); ok {
    logger = l
    logger.Debug(fmt.Sprintf("[PLUGIN: %s] 🎫 Client-Plugin: Registered", ClientRegisterer))
  }
}

func (serverRegisterer) RegisterLogger(v interface{}) {
  if l, ok := v.(Logger); ok {
    logger = l
    logger.Debug(fmt.Sprintf("[PLUGIN: %s] 🧭 Server-Plugin: Registered", HandlerRegisterer))
  }
}
