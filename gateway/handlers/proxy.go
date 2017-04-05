package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"os"

	"github.com/Sirupsen/logrus"
	"github.com/alexellis/faas/gateway/metrics"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
)

// MakeProxy creates a proxy for HTTP web requests which can be routed to a function.
func MakeProxy(metrics metrics.MetricOptions, wildcard bool, client *client.Client, logger *logrus.Logger) http.HandlerFunc {
	proxyClient := http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 0,
			}).DialContext,
			MaxIdleConns:          1,
			DisableKeepAlives:     true,
			IdleConnTimeout:       120 * time.Millisecond,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		if r.Method == "POST" {
			logger.Infoln(r.Header)

			xfunctionHeader := r.Header["X-Function"]
			if len(xfunctionHeader) > 0 {
				logger.Infoln(xfunctionHeader)
			}

			// getServiceName
			var serviceName string
			if wildcard {
				vars := mux.Vars(r)
				name := vars["name"]
				serviceName = name
			} else if len(xfunctionHeader) > 0 {
				serviceName = xfunctionHeader[0]
			}

			if len(serviceName) > 0 {
				lookupInvoke(w, r, metrics, serviceName, client, logger, &proxyClient)
			} else {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("Provide an x-function header or valid route /function/function_name."))
			}

		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func writeHead(service string, metrics metrics.MetricOptions, code int, w http.ResponseWriter) {
	w.WriteHeader(code)

	metrics.GatewayFunctionInvocation.With(prometheus.Labels{"function_name": service, "code": strconv.Itoa(code)}).Inc()

	// metrics.GatewayFunctionInvocation.WithLabelValues(service).Add(1)
}

func trackTime(then time.Time, metrics metrics.MetricOptions, name string) {
	since := time.Since(then)
	metrics.GatewayFunctionsHistogram.WithLabelValues(name).Observe(since.Seconds())
}

func lookupInvoke(w http.ResponseWriter, r *http.Request, metrics metrics.MetricOptions, name string, c *client.Client, logger *logrus.Logger, proxyClient *http.Client) {
	exists, err := lookupSwarmService(name, c)

	if err != nil || exists == false {
		if err != nil {
			logger.Infof("Could not resolve service: %s error: %s.", name, err)
		}

		// TODO: Should record the 404/not found error in Prometheus.
		writeHead(name, metrics, http.StatusNotFound, w)
		w.Write([]byte(fmt.Sprintf("Cannot find service: %s.", name)))
	}

	if exists {
		defer trackTime(time.Now(), metrics, name)
		requestBody, _ := ioutil.ReadAll(r.Body)
		invokeService(w, r, metrics, name, requestBody, logger, proxyClient)
	}
}

func lookupSwarmService(serviceName string, c *client.Client) (bool, error) {
	fmt.Printf("Resolving: '%s'\n", serviceName)
	serviceFilter := filters.NewArgs()
	serviceFilter.Add("name", serviceName)
	services, err := c.ServiceList(context.Background(), types.ServiceListOptions{Filters: serviceFilter})

	return len(services) > 0, err
}

func invokeService(w http.ResponseWriter, r *http.Request, metrics metrics.MetricOptions, service string, requestBody []byte, logger *logrus.Logger, proxyClient *http.Client) {
	stamp := strconv.FormatInt(time.Now().Unix(), 10)

	defer func(when time.Time) {
		seconds := time.Since(when).Seconds()

		fmt.Printf("[%s] took %f seconds\n", stamp, seconds)
		metrics.GatewayFunctionsHistogram.WithLabelValues(service).Observe(seconds)
	}(time.Now())

	//TODO: inject setting rather than looking up each time.
	var dnsrr bool
	if os.Getenv("dnsrr") == "true" {
		dnsrr = true
	}

	watchdogPort := 8080

	addr := service
	// Use DNS-RR via tasks.servicename if enabled as override, otherwise VIP.
	if dnsrr {
		entries, lookupErr := net.LookupIP(fmt.Sprintf("tasks.%s", service))
		if lookupErr == nil && len(entries) > 0 {
			index := randomInt(0, len(entries))
			addr = entries[index].String()
		}
	}
	url := fmt.Sprintf("http://%s:%d/", addr, watchdogPort)

	contentType := r.Header.Get("Content-Type")
	fmt.Printf("[%s] Forwarding request [%s] to: %s\n", stamp, contentType, url)

	request, err := http.NewRequest("POST", url, bytes.NewReader(requestBody))

	copyHeaders(&request.Header, &r.Header)

	defer request.Body.Close()

	response, err := proxyClient.Do(request)
	if err != nil {
		logger.Infoln(err)
		writeHead(service, metrics, http.StatusInternalServerError, w)
		buf := bytes.NewBufferString("Can't reach service: " + service)
		w.Write(buf.Bytes())
		return
	}

	responseBody, readErr := ioutil.ReadAll(response.Body)
	if readErr != nil {
		fmt.Println(readErr)

		writeHead(service, metrics, http.StatusInternalServerError, w)
		buf := bytes.NewBufferString("Error reading response from service: " + service)
		w.Write(buf.Bytes())
		return
	}

	clientHeader := w.Header()
	copyHeaders(&clientHeader, &response.Header)

	// TODO: copyHeaders removes the need for this line - test removal.
	// Match header for strict services
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))

	writeHead(service, metrics, http.StatusOK, w)
	w.Write(responseBody)
}

func copyHeaders(destination *http.Header, source *http.Header) {
	for k, vv := range *source {
		vvClone := make([]string, len(vv))
		copy(vvClone, vv)
		(*destination)[k] = vvClone
	}
}

func randomInt(min, max int) int {
	rand.Seed(time.Now().Unix())
	return rand.Intn(max-min) + min
}