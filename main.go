package main

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

var timeout = getEnvInt("TIMEOUT", 15)
var retries = getEnvInt("RETRIES", 5)
var port = os.Getenv("PORT")
var logSlowMs = getEnvInt("LOG_SLOW_MS", 300)
var logErrorsOnly = os.Getenv("LOG_ERRORS_ONLY") == "true"

func getEnvInt(key string, fallback int) int {
	val, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return val
}

var client *fasthttp.Client

func main() {
	h := func(ctx *fasthttp.RequestCtx) {
		start := time.Now()
		requestHandler(ctx)

		status := ctx.Response.StatusCode()
		durMs := time.Since(start).Milliseconds()

		if logErrorsOnly && status < 400 && durMs < int64(logSlowMs) {
			return // skip normal fast 2xx / 3xx responses
		}
		if durMs < int64(logSlowMs) && status < 400 {
			return
		}

		jlog(map[string]any{
			"at":       "request_end",
			"method":   string(ctx.Method()),
			"uri":      string(ctx.RequestURI()),
			"status":   status,
			"duration": durMs,
			"remote":   ctx.RemoteIP().String(),
		})
	}

	client = &fasthttp.Client{
		ReadTimeout:         time.Duration(timeout) * time.Second,
		MaxIdleConnDuration: 60 * time.Second,
		MaxConnsPerHost:     16,
	}

	jlog(map[string]any{
		"at":      "startup",
		"port":    port,
		"timeout": timeout,
		"retries": retries,
	})

	if err := fasthttp.ListenAndServe(":"+port, h); err != nil {
		log.Fatalf("Error in ListenAndServe: %s", err)
	}
}

func jlog(fields map[string]any) {
	b, _ := json.Marshal(fields)
	log.Println(string(b))
}

func init() {
	log.SetFlags(log.LstdFlags | log.LUTC | log.Lshortfile)
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	val, ok := os.LookupEnv("KEY")

	if ok && string(ctx.Request.Header.Peek("PROXYKEY")) != val {
		ctx.SetStatusCode(407)
		ctx.SetBody([]byte("Missing or invalid PROXYKEY header."))
		return
	}

	if len(strings.SplitN(string(ctx.Request.Header.RequestURI())[1:], "/", 2)) < 2 {
		ctx.SetStatusCode(400)
		ctx.SetBody([]byte("URL format invalid."))
		return
	}

	response := makeRequest(ctx, 1)
	defer fasthttp.ReleaseResponse(response)

	ctx.SetStatusCode(response.StatusCode())
	response.Header.VisitAll(func(key, value []byte) {
		ctx.Response.Header.Set(string(key), string(value))
	})

	ctx.Response.Header.Set("X-Proxy-Upstream-Status", strconv.Itoa(response.StatusCode()))
	ctx.Response.Header.Set("Via", "roproxy-lite")

	ctx.SetBody(response.Body())
}

func makeRequest(ctx *fasthttp.RequestCtx, attempt int) *fasthttp.Response {
	if attempt > retries {
		r := fasthttp.AcquireResponse()
		r.SetStatusCode(504)
		r.SetBodyString("upstream timeout")
		return r
	}

	raw := string(ctx.RequestURI())
	parts := strings.SplitN(raw[1:], "/", 2)
	if len(parts) < 2 {
		r := fasthttp.AcquireResponse()
		r.SetStatusCode(400)
		r.SetBodyString("URL format invalid.")
		return r
	}
	upHost, upPath := parts[0], parts[1]

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethodBytes(ctx.Method())
	req.SetRequestURI("https://" + upHost + "/" + upPath)
	req.Header.SetHost(upHost)
	req.Header.Set("User-Agent", "RoProxy")
	req.Header.Del("Roblox-Id")
	req.SetBody(ctx.Request.Body())

	ctx.Request.Header.VisitAll(func(k, v []byte) {
		switch strings.ToLower(string(k)) {
		case "host", "connection", "proxy-connection", "keep-alive",
			"transfer-encoding", "upgrade", "te", "content-length",
			"accept-encoding", "proxykey", "x-request-id":
			return
		default:
			req.Header.SetBytesKV(k, v)
		}
	})

	resp := fasthttp.AcquireResponse()
	if err := client.Do(req, resp); err != nil {
		jlog(map[string]any{"at": "retry_err", "attempt": attempt, "uri": raw, "err": err.Error()})
		fasthttp.ReleaseResponse(resp)
		time.Sleep(time.Duration(100*attempt) * time.Millisecond)
		return makeRequest(ctx, attempt+1)
	}

	sc := resp.StatusCode()
	if sc == 429 {
		if ra := resp.Header.Peek("Retry-After"); len(ra) > 0 {
			if s, _ := strconv.Atoi(string(ra)); s > 0 {
				time.Sleep(time.Duration(s)*time.Second + 100*time.Millisecond)
			}
		}
		return resp
	}
	if sc >= 500 && sc <= 599 {
		jlog(map[string]any{"at": "retry_5xx", "attempt": attempt, "status": sc, "uri": raw})
		time.Sleep(time.Duration(100*attempt) * time.Millisecond)
		resp.Reset()
		return makeRequest(ctx, attempt+1)
	}
	return resp
}
