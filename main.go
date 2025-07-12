package main

import (
	"encoding/json"
	"log"
	"time"
	"os"
	"github.com/valyala/fasthttp"
	"strconv"
	"strings"
)

var timeout = getEnvInt("TIMEOUT", 10)
var retries = getEnvInt("RETRIES", 3)
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

		// Decide whether to log
		status := ctx.Response.StatusCode()
		durMs  := time.Since(start).Milliseconds()

		if logErrorsOnly && status < 400 && durMs < int64(logSlowMs) {
			return // skip normal fast 2xx / 3xx responses
		}
		if durMs < int64(logSlowMs) && status < 400 {
			return // nothing “interesting”
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
		ReadTimeout: time.Duration(timeout) * time.Second,
		MaxIdleConnDuration: 60 * time.Second,
	}
	
    jlog(map[string]any{
        "at":      "startup",
        "port":    port,
        "timeout": timeout,
        "retries": retries,
    })

	if err := fasthttp.ListenAndServe(":" + port, h); err != nil {
		log.Fatalf("Error in ListenAndServe: %s", err)
	}
}

// Error logs
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

	body := response.Body()
	ctx.SetBody(body)
	ctx.SetStatusCode(response.StatusCode())
	response.Header.VisitAll(func (key, value []byte) {
		ctx.Response.Header.Set(string(key), string(value))
	})
}

func makeRequest(ctx *fasthttp.RequestCtx, attempt int) *fasthttp.Response {
	if attempt > retries {
		resp := fasthttp.AcquireResponse()
		resp.SetBody([]byte("Proxy failed to connect. Please try again."))
		resp.SetStatusCode(500)

		return resp
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod(string(ctx.Method()))
	url := strings.SplitN(string(ctx.Request.Header.RequestURI())[1:], "/", 2)
	req.SetRequestURI("https://" + url[0] + "/" + url[1])
	req.SetBody(ctx.Request.Body())
	ctx.Request.Header.VisitAll(func (key, value []byte) {
		req.Header.Set(string(key), string(value))
	})
	req.Header.Set("User-Agent", "RoProxy")
	req.Header.Del("Roblox-Id")
	resp := fasthttp.AcquireResponse()

	err := client.Do(req, resp)

    if err != nil {
		jlog(map[string]any{
			"at":       "retry",
			"attempt":  attempt,
			"max":      retries,
			"method":   string(ctx.Method()),
			"uri":      string(ctx.RequestURI()),
			"error":    err.Error(),
		})
		fasthttp.ReleaseResponse(resp)
		return makeRequest(ctx, attempt+1)
	}
	return resp
}