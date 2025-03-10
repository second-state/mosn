/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package extract

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mosn.io/api"
	"mosn.io/mosn/pkg/cel/attribute"
	"mosn.io/mosn/pkg/istio"
	"mosn.io/mosn/pkg/log"
	"mosn.io/mosn/pkg/protocol"
	"mosn.io/mosn/pkg/types"
	"mosn.io/mosn/pkg/variable"
	"mosn.io/pkg/buffer"
)

func ExtractAttributes(ctx context.Context, reqHeaders api.HeaderMap, respHeaders api.HeaderMap, requestInfo api.RequestInfo, buf buffer.IoBuffer, trailers api.HeaderMap, now time.Time) attribute.Bag {
	return &extractAttributes{
		ctx:         ctx,
		reqHeaders:  reqHeaders,
		respHeaders: respHeaders,
		requestInfo: requestInfo,
		buf:         buf,
		trailers:    trailers,
		now:         now,
		extracted:   map[string]interface{}{},
	}
}

type extractAttributes struct {
	ctx         context.Context
	reqHeaders  api.HeaderMap
	respHeaders api.HeaderMap
	requestInfo api.RequestInfo
	buf         buffer.IoBuffer
	trailers    api.HeaderMap
	now         time.Time
	attributes  map[string]interface{}
	extracted   map[string]interface{}
}

func (e *extractAttributes) Get(name string) (interface{}, bool) {
	v, ok := e.extracted[name]
	if ok {
		if v == nil {
			return nil, false
		}
		return v, ok
	}

	switch name {
	case istio.KOriginIP:
		addr := e.requestInfo.DownstreamLocalAddress()
		if addr != nil {
			ip, _, ret := getIPPort(addr.String())
			if ret {
				v := net.ParseIP(ip)
				e.extracted[istio.KOriginIP] = v
				return v, true
			}
		}
		e.extracted[istio.KOriginIP] = nil
	case istio.KRequestTime:
		return e.requestInfo.StartTime(), true
	case istio.KContextProtocol:
		proto := "http"
		switch t := e.requestInfo.Protocol(); t {
		case protocol.HTTP1:
			proto = "http"
		case protocol.HTTP2:
			proto = "h2"
		default:
			if t != "" {
				proto = string(t)
			}
		}
		e.extracted[istio.KContextProtocol] = proto
		return proto, true
	case istio.KDestinationIP, istio.KDestinationPort:
		hostInfo := e.requestInfo.UpstreamHost()
		if hostInfo != nil {
			address := hostInfo.AddressString()
			ip, port, ret := getIPPort(address)
			if ret {
				e.extracted[istio.KDestinationIP] = net.ParseIP(ip)
				e.extracted[istio.KDestinationPort] = int64(port)
				return e.extracted[name], true
			}
		}
		e.extracted[istio.KDestinationIP] = nil
		e.extracted[istio.KDestinationPort] = nil
	case istio.KRequestHeaders:
		return e.reqHeaders, true
	case istio.KResponseHeaders:
		return e.respHeaders, true
	case istio.KResponseTime:
		return e.now, true
	case istio.KRequestBodySize:
		return int64(e.requestInfo.BytesReceived()), true
	case istio.KResponseBodySize:
		return int64(e.requestInfo.BytesSent()), true
	case istio.KRequestTotalSize:
		var sum int64
		if e.reqHeaders != nil {
			sum += int64(e.reqHeaders.ByteSize())
		}
		if e.buf != nil {
			sum += int64(e.buf.Len())
		}
		if e.trailers != nil {
			sum += int64(e.trailers.ByteSize())
		}
		e.extracted[istio.KRequestTotalSize] = sum
		return sum, true
	case istio.KResponseTotalSize:
		return int64(e.requestInfo.BytesSent() + e.respHeaders.ByteSize()), true
	case istio.KResponseDuration:
		return e.requestInfo.Duration(), true
	case istio.KResponseCode:
		return int64(e.requestInfo.ResponseCode()), true
	case istio.KRequestPath:
		path, err := variable.GetString(e.ctx, types.VarPath)
		if err != nil || path == "" {
			return nil, false
		}
		return path, true
	case istio.KRequestQueryParms:
		query, err := variable.GetString(e.ctx, types.VarQueryString)
		if err == nil && query != "" {
			v, err := parseQuery(query)
			if err == nil {
				v := protocol.CommonHeader(v)
				e.extracted[istio.KRequestQueryParms] = v
				return e.extracted[name], true
			}
		}
		e.extracted[istio.KRequestQueryParms] = nil
	case istio.KRequestUrlPath:
		path, err := variable.GetString(e.ctx, types.VarPath)
		if err == nil && path != "" {
			query, err := variable.GetString(e.ctx, types.VarQueryString)
			if err == nil && query != "" {
				url := path + "?" + query
				e.extracted[istio.KRequestUrlPath] = url
				return e.extracted[name], true
			}
			e.extracted[istio.KRequestUrlPath] = path
			return e.extracted[name], true
		}
		e.extracted[istio.KRequestUrlPath] = nil
	case istio.KRequestMethod:
		method, err := variable.GetString(e.ctx, types.VarMethod)
		if err != nil || method == "" {
			return nil, false
		}
		return method, true
	case istio.KRequestHost:
		host, err := variable.GetString(e.ctx, types.VarHost)
		if err != nil || host == "" {
			return nil, false
		}
		return host, true
	case istio.KDestinationServiceHost, istio.KDestinationServiceName, istio.KDestinationServiceNamespace, istio.KContextReporterKind:
		routeEntry := e.requestInfo.RouteEntry()
		if routeEntry != nil {
			clusterName := routeEntry.ClusterName(e.ctx)
			if clusterName != "" {
				info := paresClusterName(clusterName)
				e.extracted[istio.KDestinationServiceHost] = info.Host
				e.extracted[istio.KDestinationServiceName] = info.Name
				e.extracted[istio.KDestinationServiceNamespace] = info.Namespace
				e.extracted[istio.KContextReporterKind] = info.Kind
				return e.extracted[name], true
			}
		}
		fallthrough
	default:
		if e.attributes == nil {
			val, ret := e.reqHeaders.Get(istio.KIstioAttributeHeader)
			if ret {
				e.attributes = generateDefaultAttributes(val)
			}
			// generateDefaultAttributes maybe returns a nil
			if e.attributes == nil {
				e.attributes = map[string]interface{}{}
			}
		}
		v, ok = e.attributes[name]
		if ok {
			return v, ok
		}
	}
	return nil, false
}

var defaultAttributeGenerator = func(s string) map[string]interface{} {
	log.DefaultLogger.Warnf("[cel] no defaul attribute generator functions.")
	return nil
}

func RegisterAttributeGenerator(f func(s string) map[string]interface{}) {
	defaultAttributeGenerator = f
}

func generateDefaultAttributes(s string) map[string]interface{} {
	return defaultAttributeGenerator(s)
}

// getIPPort return ip and port of address
func getIPPort(address string) (ip string, port int32, ret bool) {
	ret = false
	array := strings.Split(address, ":")
	if len(array) != 2 {
		return
	}

	p, err := strconv.Atoi(array[1])
	if err != nil {
		return
	}

	ip = array[0]
	port = int32(p)
	ret = true
	return
}

var outbound = "outbound"

type clusterNameInfo struct {
	Kind      string
	Version   string
	Name      string
	Namespace string
	Host      string
}

func paresClusterName(clusterName string) *clusterNameInfo {
	c := &clusterNameInfo{}
	items := strings.Split(clusterName, "|")
	if len(items) == 4 {
		c.Kind = items[0]
		if outbound == c.Kind {
			c.Version = items[2]
		}
		c.Host = items[3]
		serviceItems := strings.SplitN(c.Host, ".", 3)
		if len(serviceItems) == 3 {
			c.Name = serviceItems[0]
			c.Namespace = serviceItems[1]
		}
	}
	return c
}

func parseQuery(query string) (m map[string]string, err error) {
	m = map[string]string{}
	for query != "" {
		key := query
		if i := strings.IndexAny(key, "&;"); i >= 0 {
			key, query = key[:i], key[i+1:]
		} else {
			query = ""
		}
		if key == "" {
			continue
		}
		value := ""
		if i := strings.Index(key, "="); i >= 0 {
			key, value = key[:i], key[i+1:]
		}
		key, err1 := url.QueryUnescape(key)
		if err1 != nil {
			if err == nil {
				err = err1
			}
			continue
		}
		// Only the first value is taken by default
		if _, ok := m[key]; ok {
			continue
		}
		value, err1 = url.QueryUnescape(value)
		if err1 != nil {
			if err == nil {
				err = err1
			}
			continue
		}

		m[key] = value
	}
	return m, err
}
