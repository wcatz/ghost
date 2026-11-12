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

func withheldResourceURI(uri string) (string, bool) {
	for _, form := range []string{uri, lenientUnescape(uri)} {
		if shown := memory.RefusalArg("resource URI", form); shown != strconv.Quote(form) {
			return shown, true
		}
	}
	return "", false
}

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
