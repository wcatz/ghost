package mcpserver

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// withholdResourceReadURI is the receiving middleware that keeps a resources/read
// refusal from repeating a token-shaped URI. A URI that matches no registered
// resource or template never reaches a Ghost handler: the SDK answers it itself
// with `Resource not found` and a `data.uri` holding the URI as sent, and a
// project segment is routinely a clone URL, so that echo bypassed every
// `memory.RefusalArg` on this path. A bad escape, a raw '/' in the segment and an
// unknown scheme all take it.
//
// The URI is judged, never searched for in Data: Data quotes it with Go's `%q`,
// whose escaping can differ from the raw string. When RefusalArg would withhold
// the URI or its decoded form (the same `!= strconv.Quote` test the project
// not-found refusal uses, so an oversized URI counts too), the error goes out
// without Data and with every copy of the URI in its message replaced by the
// placeholder. The rebuilt error is what the SDK's wire encoding would have sent
// anyway — the message of the outermost error and the code of the wire error it
// wraps — less the Data. An error whose URI holds no token passes untouched, and
// one already withheld stays withheld: nothing here adds text back.
func withholdResourceReadURI(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err == nil || method != "resources/read" {
			return res, err
		}
		read, ok := req.(*mcp.ReadResourceRequest)
		if !ok || read.Params == nil {
			return res, err
		}
		uri := read.Params.URI
		shown, withheld := withheldResourceURI(uri)
		if !withheld {
			return res, err
		}
		out := &jsonrpc.Error{Message: err.Error()}
		var wire *jsonrpc.Error
		if errors.As(err, &wire) {
			out.Code = wire.Code
		}
		for _, form := range []string{uri, lenientUnescape(uri)} {
			out.Message = strings.ReplaceAll(out.Message, strconv.Quote(form), shown)
			out.Message = strings.ReplaceAll(out.Message, form, shown)
		}
		return nil, out
	}
}

// withheldResourceURI reports whether memory.RefusalArg withholds uri, judged on
// the URI as sent and on its decoded form, and returns the placeholder to name it
// by. Both forms are needed: a percent-encoded clone URL hides a token behind
// `%2F`, where the guard's word boundary does not fire on the raw string.
func withheldResourceURI(uri string) (string, bool) {
	for _, form := range []string{uri, lenientUnescape(uri)} {
		if shown := memory.RefusalArg("resource URI", form); shown != strconv.Quote(form) {
			return shown, true
		}
	}
	return "", false
}

// lenientUnescape decodes every well-formed %XX in s and leaves anything else as
// it is. url.PathUnescape refuses the whole string on one bad escape, and a bad
// escape is exactly one of the URIs that never reaches a handler, so it would
// leave an encoded token beside it undecoded.
func lenientUnescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
