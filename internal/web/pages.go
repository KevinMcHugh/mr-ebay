package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/auth"
	"github.com/KevinMcHugh/mr-ebay/internal/server"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// The pages drive the same endpoints as the JSON API and MCP, through
// server.BuildViewModel, and render the view models as HTML.

func parsePages() map[string]*template.Template {
	funcs := template.FuncMap{
		"money":         money,
		"dollars":       dollars,
		"join":          strings.Join,
		"has":           slices.Contains[[]string],
		"conditions":    func() []string { return searches.Conditions },
		"buyingOptions": func() []string { return searches.BuyingOptions },
	}
	pages := map[string]*template.Template{}
	for _, name := range []string{"login", "index", "search"} {
		pages[name] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/form.html", "templates/"+name+".html"))
	}
	return pages
}

// money is cents of a currency for reading: "12.50 USD".
func money(cents int64, currency string) string {
	return dollars(cents) + " " + currency
}

// dollars is cents as a decimal amount, "" for 0 (no limit), for a form
// field.
func dollars(cents int64) string {
	if cents == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// parseCents reads a form's decimal amount ("12", "12.5", "12.50") as
// cents; blank is 0, which means no limit.
func parseCents(field, s string) (int64, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	if s == "" {
		return 0, nil
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("%w: %s %q has more than two decimal places", app.ErrBadInput, field, s)
	}
	frac = (frac + "00")[:2]
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w < 0 {
		return 0, fmt.Errorf("%w: %s %q is not an amount like 12.50", app.ErrBadInput, field, s)
	}
	return w*100 + f, nil
}

// render writes a page through a buffer, so a template error becomes a 500
// rather than half a page. Every page learns whether the visitor is signed
// in (the public pages hide their forms from anyone who is not) and gets the
// one-line message a redirect left in ?msg= or ?err=.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	data["AppName"] = app.Name
	data["SignedIn"] = auth.KeyID(r.Context()) != ""
	data["Path"] = r.URL.RequestURI()
	if _, ok := data["Flash"]; !ok {
		data["Flash"] = r.URL.Query().Get("msg")
	}
	if _, ok := data["Error"]; !ok {
		data["Error"] = r.URL.Query().Get("err")
	}
	var buf bytes.Buffer
	if err := s.pages[name].Execute(&buf, data); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// statusOf maps the endpoints' errors to HTTP statuses.
func statusOf(err error) int {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, app.ErrBadInput):
		return http.StatusUnprocessableEntity
	case errors.Is(err, app.ErrUpstream):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// fail answers a page request whose endpoint failed.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := statusOf(err)
	if status == http.StatusInternalServerError {
		slog.Error("page", "path", r.URL.Path, "err", err)
		http.Error(w, "internal error", status)
		return
	}
	http.Error(w, err.Error(), status)
}

// redirect sends the browser to path with a one-line message, err or not,
// that the next page shows (post/redirect/get, with no script and no
// session state).
func redirect(w http.ResponseWriter, r *http.Request, path, msg string, isErr bool) {
	key := "msg"
	if isErr {
		key = "err"
	}
	http.Redirect(w, r, path+"?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
}

// ---- searches ---------------------------------------------------------------

func (s *Server) indexPage(w http.ResponseWriter, r *http.Request) {
	list, err := server.BuildViewModel(r.Context(), searches.ListEndpoint{Store: s.Store, Links: s.Links}, apigen.ListSearchesRequestObject{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "index", map[string]any{
		"Searches": list,
		"Form":     searches.Search{Currency: "USD", Marketplace: "EBAY_US"},
	})
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request) {
	sr, err := server.BuildViewModel(r.Context(), searches.GetEndpoint{Store: s.Store, Links: s.Links}, apigen.GetSearchRequestObject{SearchId: chi.URLParam(r, "id")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "search", map[string]any{"Search": sr, "Form": sr})
}

// searchForm reads the search form. Every field is on the form, so it is
// a whole search, for create and update alike.
func searchForm(r *http.Request) (apigen.SearchCreate, error) {
	if err := r.ParseForm(); err != nil {
		return apigen.SearchCreate{}, fmt.Errorf("%w: %v", app.ErrBadInput, err)
	}
	var cents [3]int64
	for i, field := range []string{"minPrice", "maxPrice", "alertBelow"} {
		c, err := parseCents(field, r.FormValue(field))
		if err != nil {
			return apigen.SearchCreate{}, err
		}
		cents[i] = c
	}
	var cats []string
	for _, c := range strings.Split(r.FormValue("categoryIds"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cats = append(cats, c)
		}
	}
	return apigen.SearchCreate{
		Name: r.FormValue("name"), Query: r.FormValue("query"), CategoryIds: cats,
		Conditions: r.Form["conditions"], BuyingOptions: r.Form["buyingOptions"],
		MinPriceCents: cents[0], MaxPriceCents: cents[1], AlertBelowCents: cents[2],
		Currency: r.FormValue("currency"), Marketplace: r.FormValue("marketplace"),
		Wants: r.FormValue("wants"), Watch: r.FormValue("watch") != "",
	}, nil
}

func (s *Server) createSearch(w http.ResponseWriter, r *http.Request) {
	body, err := searchForm(r)
	if err == nil {
		var sr searches.Search
		sr, err = server.BuildViewModel(r.Context(), searches.CreateEndpoint{Store: s.Store, Links: s.Links}, apigen.CreateSearchRequestObject{Body: &body})
		if err == nil {
			redirect(w, r, "/searches/"+url.PathEscape(sr.ID), "Saved.", false)
			return
		}
	}
	if errors.Is(err, app.ErrBadInput) {
		redirect(w, r, "/", err.Error(), true)
		return
	}
	s.fail(w, r, err)
}

func (s *Server) updateSearch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	b, err := searchForm(r)
	if err == nil {
		_, err = server.BuildViewModel(r.Context(), searches.UpdateEndpoint{Store: s.Store, Links: s.Links}, apigen.UpdateSearchRequestObject{
			SearchId: id,
			Body: &apigen.SearchUpdate{
				Name: &b.Name, Query: &b.Query, CategoryIds: &b.CategoryIds, Conditions: &b.Conditions,
				BuyingOptions: &b.BuyingOptions, MinPriceCents: &b.MinPriceCents, MaxPriceCents: &b.MaxPriceCents,
				Currency: &b.Currency, Marketplace: &b.Marketplace, Wants: &b.Wants, Watch: &b.Watch,
				AlertBelowCents: &b.AlertBelowCents,
			},
		})
	}
	if errors.Is(err, app.ErrBadInput) {
		redirect(w, r, "/searches/"+url.PathEscape(id), err.Error(), true)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/searches/"+url.PathEscape(id), "Saved.", false)
}

func (s *Server) deleteSearch(w http.ResponseWriter, r *http.Request) {
	if _, err := server.BuildViewModel(r.Context(), searches.DeleteEndpoint{Store: s.Store}, apigen.DeleteSearchRequestObject{SearchId: chi.URLParam(r, "id")}); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/", "Deleted.", false)
}
