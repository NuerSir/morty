package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"
)

const (
	STATE_DEFAULT     int = 0
	STATE_IN_STYLE    int = 1
	STATE_IN_NOSCRIPT int = 2
)

var CLIENT *fasthttp.Client = &fasthttp.Client{
	MaxResponseBodySize: 10 * 1024 * 1024, // 10M
}

var CSS_URL_REGEXP *regexp.Regexp = regexp.MustCompile("url\\((['\"]?)([\u0009\u0021\u0023-\u0026\u0028\u002a-\u007E]+)(['\"]?)\\)")

var UNSAFE_ELEMENTS [][]byte = [][]byte{
	[]byte("applet"),
	[]byte("canvas"),
	[]byte("embed"),
	//[]byte("iframe"),
	[]byte("script"),
}

var SAFE_ATTRIBUTES [][]byte = [][]byte{
	[]byte("abbr"),
	[]byte("accesskey"),
	[]byte("align"),
	[]byte("alt"),
	[]byte("autocomplete"),
	[]byte("charset"),
	[]byte("checked"),
	[]byte("class"),
	[]byte("content"),
	[]byte("contenteditable"),
	[]byte("contextmenu"),
	[]byte("dir"),
	[]byte("for"),
	[]byte("height"),
	[]byte("hidden"),
	[]byte("id"),
	[]byte("lang"),
	[]byte("media"),
	[]byte("method"),
	[]byte("name"),
	[]byte("nowrap"),
	[]byte("placeholder"),
	[]byte("property"),
	[]byte("rel"),
	[]byte("spellcheck"),
	[]byte("tabindex"),
	[]byte("target"),
	[]byte("title"),
	[]byte("translate"),
	[]byte("type"),
	[]byte("value"),
	[]byte("width"),
}

var SELF_CLOSING_ELEMENTS [][]byte = [][]byte{
	[]byte("area"),
	[]byte("base"),
	[]byte("br"),
	[]byte("col"),
	[]byte("embed"),
	[]byte("hr"),
	[]byte("img"),
	[]byte("input"),
	[]byte("keygen"),
	[]byte("link"),
	[]byte("meta"),
	[]byte("param"),
	[]byte("source"),
	[]byte("track"),
	[]byte("wbr"),
}

type Proxy struct {
	Key []byte
}

type RequestConfig struct {
	Key     []byte
	baseURL *url.URL
}

var HTML_FORM_EXTENSION string = `<input type="hidden" name="mortyurl" value="%s" /><input type="hidden" name="mortyhash" value="%s" />`

var HTML_BODY_EXTENSION string = `
</div>
<div id="mortyheader">
  <input type="checkbox" id="mortytoggle" autocomplete="off" />
  <div><p>Proxified view,<br />visit <a href="%s">original site</a>.</p><p><label for="mortytoggle">hide</label></p></div>
</div>
<style>
#mortyheader { position: fixed; top: 15%%; left: 0; max-width: 10em; color: #444; overflow: hidden; z-index: 1000; }
#mortyheader a { color: #3498db; }
#mortyheader div { padding: 8px; font-size: 0.9em; border-width: 4px 4px 4px 0; border-style: solid; border-color: #3498db; background: #FFF; }
#mortyheader label { text-align: right; }
input[type=checkbox]#mortytoggle { display: none; }
input[type=checkbox]#mortytoggle:checked ~ div { display: none; }
</style>
`

func (p *Proxy) RequestHandler(ctx *fasthttp.RequestCtx) {
	requestHash := popRequestParam(ctx, []byte("mortyhash"))

	requestURI := popRequestParam(ctx, []byte("mortyurl"))

	if requestURI == nil {
		p.breakOnError(ctx, errors.New(`missing "mortyurl" URL parameter`))
		return
	}

	if p.Key != nil {
		if !verifyRequestURI(requestURI, requestHash, p.Key) {
			p.breakOnError(ctx, errors.New("invalid hash"))
			return
		}
	}

	parsedURI, err := url.Parse(string(requestURI))

	if p.breakOnError(ctx, err) {
		return
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	reqQuery := parsedURI.Query()
	ctx.QueryArgs().VisitAll(func(key, value []byte) {
		reqQuery.Add(string(key), string(value))
	})

	parsedURI.RawQuery = reqQuery.Encode()

	uriStr := parsedURI.String()

	log.Println("getting", uriStr)

	req.SetRequestURI(uriStr)
	req.Header.SetUserAgentBytes([]byte("Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/53.0.2785.143 Safari/537.36"))

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethodBytes(ctx.Method())
	if ctx.IsPost() || ctx.IsPut() {
		req.SetBody(ctx.PostBody())
	}

	if p.breakOnError(ctx, CLIENT.Do(req, resp)) {
		return
	}

	if resp.StatusCode() != 200 {
		switch resp.StatusCode() {
		case 301, 302:
			loc := resp.Header.Peek("Location")
			if loc != nil {
				url, err := proxifyURI(&RequestConfig{p.Key, parsedURI}, string(loc))
				if err == nil {
					ctx.SetStatusCode(resp.StatusCode())
					ctx.Response.Header.Add("Location", url)
					log.Println("redirect to", string(loc))
					return
				}
			}
		}
		log.Println("invalid request:", resp.StatusCode())
		return
	}

	contentType := resp.Header.Peek("Content-Type")

	if contentType == nil {
		p.breakOnError(ctx, errors.New("invalid content type"))
		return
	}

	contentInfo := bytes.SplitN(contentType, []byte(";"), 2)

	var responseBody []byte

	if len(contentInfo) == 2 && bytes.Contains(contentInfo[1], []byte("ISO-8859-2")) && bytes.Contains(contentInfo[0], []byte("text")) {
		var err error
		responseBody, err = charmap.ISO8859_2.NewDecoder().Bytes(resp.Body())
		if p.breakOnError(ctx, err) {
			return
		}
	} else {
		responseBody = resp.Body()
	}

	ctx.SetContentType(fmt.Sprintf("%s; charset=UTF-8", contentInfo[0]))

	switch {
	case bytes.Contains(contentType, []byte("css")):
		sanitizeCSS(&RequestConfig{p.Key, parsedURI}, ctx, responseBody)
	case bytes.Contains(contentType, []byte("html")):
		sanitizeHTML(&RequestConfig{p.Key, parsedURI}, ctx, responseBody)
	default:
		ctx.Write(responseBody)
	}

}

func popRequestParam(ctx *fasthttp.RequestCtx, paramName []byte) []byte {
	param := ctx.QueryArgs().PeekBytes(paramName)

	if param == nil {
		param = ctx.PostArgs().PeekBytes(paramName)
		if param != nil {
			ctx.PostArgs().DelBytes(paramName)
		}
	} else {
		ctx.QueryArgs().DelBytes(paramName)
	}

	return param
}

func sanitizeCSS(rc *RequestConfig, ctx *fasthttp.RequestCtx, css []byte) {
	// TODO

	urlSlices := CSS_URL_REGEXP.FindAllSubmatchIndex(css, -1)

	if urlSlices == nil {
		ctx.Write(css)
		return
	}

	startIndex := 0

	for _, s := range urlSlices {
		urlStart := s[4]
		urlEnd := s[5]

		if uri, err := proxifyURI(rc, string(css[urlStart:urlEnd])); err == nil {
			ctx.Write(css[startIndex:urlStart])
			ctx.Write([]byte(uri))
			startIndex = urlEnd
		} else {
			log.Println("cannot proxify css uri:", css[urlStart:urlEnd])
		}
	}
	if startIndex < len(css) {
		ctx.Write(css[startIndex:len(css)])
	}
}

func sanitizeHTML(rc *RequestConfig, ctx *fasthttp.RequestCtx, htmlDoc []byte) {
	r := bytes.NewReader(htmlDoc)
	decoder := html.NewTokenizer(r)
	decoder.AllowCDATA(true)

	unsafeElements := make([][]byte, 0, 8)
	state := STATE_DEFAULT

	for {
		token := decoder.Next()
		if token == html.ErrorToken {
			err := decoder.Err()
			if err != io.EOF {
				log.Println("failed to parse HTML:")
			}
			break
		}

		if len(unsafeElements) == 0 {

			switch token {
			case html.StartTagToken, html.SelfClosingTagToken:
				tag, hasAttrs := decoder.TagName()
				safe := !inArray(tag, UNSAFE_ELEMENTS)
				if !safe {
					if !inArray(tag, SELF_CLOSING_ELEMENTS) {
						var unsafeTag []byte = make([]byte, len(tag))
						copy(unsafeTag, tag)
						unsafeElements = append(unsafeElements, unsafeTag)
					}
					break
				}
				if bytes.Equal(tag, []byte("noscript")) {
					state = STATE_IN_NOSCRIPT
					break
				}
				var attrs [][][]byte
				fmt.Fprintf(ctx, "<%s", tag)
				if hasAttrs {
					for {
						attrName, attrValue, moreAttr := decoder.TagAttr()
						attrs = append(attrs, [][]byte{attrName, attrValue})
						if !moreAttr {
							break
						}
					}
					if bytes.Equal(tag, []byte("meta")) {
						sanitizeMetaAttrs(rc, ctx, attrs)
					} else {
						sanitizeAttrs(rc, ctx, attrs)
					}
				}
				if token == html.SelfClosingTagToken {
					fmt.Fprintf(ctx, " />")
				} else {
					fmt.Fprintf(ctx, ">")
					if bytes.Equal(tag, []byte("style")) {
						state = STATE_IN_STYLE
					}
				}
				if bytes.Equal(tag, []byte("form")) {
					var formURL *url.URL
					for _, attr := range attrs {
						if bytes.Equal(attr[0], []byte("action")) {
							formURL, _ = url.Parse(string(attr[1]))
							mergeURIs(rc.baseURL, formURL)
							break
						}
					}
					if formURL == nil {
						formURL = rc.baseURL
					}
					urlStr := formURL.String()
					var key string
					if rc.Key != nil {
						key = hash(urlStr, rc.Key)
					}
					fmt.Fprintf(ctx, HTML_FORM_EXTENSION, urlStr, key)

				}

			case html.EndTagToken:
				tag, _ := decoder.TagName()
				writeEndTag := true
				switch string(tag) {
				case "body":
					fmt.Fprintf(ctx, HTML_BODY_EXTENSION, rc.baseURL.String())
				case "style":
					state = STATE_DEFAULT
				case "noscript":
					state = STATE_DEFAULT
					writeEndTag = false
				}
				// skip noscript tags - only the tag, not the content, because javascript is sanitized
				if writeEndTag {
					fmt.Fprintf(ctx, "</%s>", tag)
				}

			case html.TextToken:
				switch state {
				case STATE_DEFAULT:
					fmt.Fprintf(ctx, "%s", decoder.Raw())
				case STATE_IN_STYLE:
					sanitizeCSS(rc, ctx, decoder.Raw())
				case STATE_IN_NOSCRIPT:
					sanitizeHTML(rc, ctx, decoder.Raw())
				}

			case html.DoctypeToken, html.CommentToken:
				ctx.Write(decoder.Raw())
			}
		} else {
			switch token {
			case html.StartTagToken:
				tag, _ := decoder.TagName()
				if inArray(tag, UNSAFE_ELEMENTS) {
					unsafeElements = append(unsafeElements, tag)
				}

			case html.EndTagToken:
				tag, _ := decoder.TagName()
				if bytes.Equal(unsafeElements[len(unsafeElements)-1], tag) {
					unsafeElements = unsafeElements[:len(unsafeElements)-1]
				}
			}
		}
	}
}

func sanitizeMetaAttrs(rc *RequestConfig, ctx *fasthttp.RequestCtx, attrs [][][]byte) {
	var http_equiv []byte
	var content []byte

	for _, attr := range attrs {
		attrName := attr[0]
		attrValue := attr[1]
		if bytes.Equal(attrName, []byte("http-equiv")) {
			http_equiv = bytes.ToLower(attrValue)
		}
		if bytes.Equal(attrName, []byte("content")) {
			content = attrValue
		}
	}

	if bytes.Equal(http_equiv, []byte("refresh")) && bytes.Index(content, []byte(";url=")) != -1 {
		parts := bytes.SplitN(content, []byte(";url="), 2)
		if uri, err := proxifyURI(rc, string(parts[1])); err == nil {
			fmt.Fprintf(ctx, ` http-equiv="refresh" content="%s;%s"`, parts[0], uri)
		}
	} else {
		sanitizeAttrs(rc, ctx, attrs)
	}

}

func sanitizeAttrs(rc *RequestConfig, ctx *fasthttp.RequestCtx, attrs [][][]byte) {
	for _, attr := range attrs {
		sanitizeAttr(rc, ctx, attr[0], attr[1])
	}
}

func sanitizeAttr(rc *RequestConfig, ctx *fasthttp.RequestCtx, attrName, attrValue []byte) {
	if inArray(attrName, SAFE_ATTRIBUTES) {
		fmt.Fprintf(ctx, " %s=\"%s\"", attrName, attrValue)
		return
	}
	switch string(attrName) {
	case "src", "href", "action":
		if uri, err := proxifyURI(rc, string(attrValue)); err == nil {
			fmt.Fprintf(ctx, " %s=\"%s\"", attrName, uri)
		} else {
			log.Println("cannot proxify uri:", attrValue)
		}
	case "style":
		fmt.Fprintf(ctx, " %s=\"", attrName)
		sanitizeCSS(rc, ctx, attrValue)
		ctx.Write([]byte("\""))
	}
}

func mergeURIs(u1, u2 *url.URL) {
	if u2.Scheme == "" || u2.Scheme == "//" {
		u2.Scheme = u1.Scheme
	}
	if u2.Host == "" && u1.Path != "" {
		u2.Host = u1.Host
		if len(u2.Path) == 0 || u2.Path[0] != '/' {
			u2.Path = path.Join(u1.Path[:strings.LastIndexByte(u1.Path, byte('/'))], u2.Path)
		}
	}
}

func proxifyURI(rc *RequestConfig, uri string) (string, error) {
	// TODO check malicious data: - e.g. data:script
	if strings.HasPrefix(uri, "data:") {
		return uri, nil
	}

	if len(uri) > 0 && uri[0] == '#' {
		return uri, nil
	}

	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	mergeURIs(rc.baseURL, u)

	uri = u.String()

	if rc.Key == nil {
		return fmt.Sprintf("./?mortyurl=%s", url.QueryEscape(uri)), nil
	}
	return fmt.Sprintf("./?mortyhash=%s&mortyurl=%s", hash(uri, rc.Key), url.QueryEscape(uri)), nil
}

func inArray(b []byte, a [][]byte) bool {
	for _, b2 := range a {
		if bytes.Equal(b, b2) {
			return true
		}
	}
	return false
}

func hash(msg string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyRequestURI(uri, hashMsg, key []byte) bool {
	h := make([]byte, hex.DecodedLen(len(hashMsg)))
	_, err := hex.Decode(h, hashMsg)
	if err != nil {
		log.Println("hmac error:", err)
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(uri)
	return hmac.Equal(h, mac.Sum(nil))
}

func (p *Proxy) breakOnError(ctx *fasthttp.RequestCtx, err error) bool {
	if err == nil {
		return false
	}
	ctx.SetStatusCode(404)
	ctx.SetContentType("text/html")
	ctx.Write([]byte(`<!doctype html>
<head>
<title>MortyError</title>
</head>
<body><h2>Error!</h2>`))
	ctx.Write([]byte("<h3>"))
	ctx.Write([]byte(html.EscapeString(err.Error())))
	ctx.Write([]byte("</h3>"))
	if p.Key == nil {
		ctx.Write([]byte(`
<form action="post">
	Visit url: <input placeholder="https://url.." name="mortyurl" />
	<input type="submit" value="go" />
</form>`))
	}
	ctx.Write([]byte(`
</body>
</html>`))
	return true
}

func main() {

	listen := flag.String("listen", "127.0.0.1:3000", "Listen address")
	key := flag.String("key", "", "HMAC url validation key (hexadecimal encoded) - leave blank to disable")
	flag.Parse()

	p := &Proxy{}

	if *key != "" {
		p.Key = []byte(*key)
	}

	log.Println("listening on", *listen)

	if err := fasthttp.ListenAndServe(*listen, p.RequestHandler); err != nil {
		log.Fatal("Error in ListenAndServe:", err)
	}
}