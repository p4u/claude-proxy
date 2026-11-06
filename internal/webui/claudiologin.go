package webui

// The browser half of `claudio proxy login` (the claudio half is
// claudioapi/login.go). A member opens /claudio/login, signs in through SSO,
// types the code their terminal shows and approves it. The page never holds a
// token: approval stores the member's user-token id on the pending login and
// claudio's poll reads the token.

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	vocauth "github.com/vocdoni/oauth/go"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

// claudioLoginPath is the approval page; claudioapi's verify_url points here.
const claudioLoginPath = "/claudio/login"

// LoginApprover is the pending-login store of the claudio API
// (*claudioapi.Logins). Codes are matched ignoring case, dashes and spaces.
type LoginApprover interface {
	// Check reports whether code names a pending, unexpired login.
	Check(code string) bool
	// Approve marks the login approved for the user token userID; false when
	// the code is unknown, expired or already decided.
	Approve(code, userID string) bool
	// Deny marks the login denied; false as for Approve.
	Deny(code string) bool
}

// MemberLimit is the usage limit given to a member created at their first
// claudio login (CLAUDIO_DEFAULT_LIMIT). Zero values mean unlimited.
type MemberLimit struct {
	OutputTokens  int64
	WindowSeconds int64
}

// LoginURL is the claudio approval page as a member's browser reaches it.
func (s *SSO) LoginURL() string { return s.origin + claudioLoginPath }

// inLoginDomain reports whether email belongs to one of the login domains.
// The session was admitted against the same list; this re-check keeps a
// cookie minted under an older configuration from outliving it.
func (s *SSO) inLoginDomain(email string) bool {
	at := strings.LastIndexByte(email, '@')
	return at > 0 && s.domains[strings.ToLower(email[at+1:])]
}

// loginPostSameOrigin is sameOrigin for the approval form. The page is
// served with Referrer-Policy: no-referrer, which makes browsers send
// "Origin: null" on its own form posts, so a null origin is accepted only
// with Sec-Fetch-Site: same-origin (a header no page can forge).
func (s *SSO) loginPostSameOrigin(r *http.Request) bool {
	if r.Header.Get("Origin") == "null" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return s.sameOrigin(r)
}

// setClaudioLoginHeaders keeps the page out of frames and caches and gives it
// no script, no remote resources and no form target but itself.
func setClaudioLoginHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
}

// serveClaudioLogin answers GET and POST /claudio/login. Without SSO or the
// claudio API's login store it is a 404, like /api/login under SSO.
func (s *Server) serveClaudioLogin(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil || s.logins == nil {
		http.NotFound(w, r)
		return
	}
	setClaudioLoginHeaders(w.Header())
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// Require (not user()): an unauthenticated browser is redirected to
		// /auth/login?return_to=/claudio/login and comes back signed in.
		s.sso.auth.Require(http.HandlerFunc(s.claudioLoginForm)).ServeHTTP(w, r)
	case http.MethodPost:
		// A missing session answers 401.
		s.sso.auth.Require(http.HandlerFunc(s.claudioLoginPost)).ServeHTTP(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) claudioLoginForm(w http.ResponseWriter, r *http.Request) {
	u, _ := vocauth.FromContext(r.Context())
	if !s.sso.inLoginDomain(u.Email) {
		renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
			Message: "This account cannot sign in to claudio."})
		return
	}
	renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "form", Email: u.Email})
}

func (s *Server) claudioLoginPost(w http.ResponseWriter, r *http.Request) {
	u, _ := vocauth.FromContext(r.Context())
	if !s.sso.loginPostSameOrigin(r) {
		renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
			Message: "Cross-origin request refused. If you are on the proxy's own page, your browser is too old for this flow " +
				"(it sends no Sec-Fetch-Site header); use a current Chrome, Firefox or Safari 16.4+."})
		return
	}
	if !s.sso.inLoginDomain(u.Email) {
		renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
			Message: "This account cannot sign in to claudio."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		renderClaudioLogin(w, http.StatusBadRequest, loginPage{Kind: "error", Email: u.Email, Message: "Bad request."})
		return
	}
	code := displayCode(r.PostForm.Get("code"))
	unknown := loginPage{Kind: "error", Email: u.Email,
		Message: "This code is unknown, expired or already used. Run claudio proxy login again."}
	if code == "" || !s.logins.Check(code) {
		renderClaudioLogin(w, http.StatusBadRequest, unknown)
		return
	}
	switch r.PostForm.Get("action") {
	case "lookup":
		renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "confirm", Email: u.Email, Code: code,
			Hidden: strings.ReplaceAll(code, "-", "")})
	case "approve":
		ut, err := s.memberToken(r.Context(), u.Email)
		if err != nil {
			slog.Error("claudio login: member lookup failed", "email", u.Email, "err", err)
			renderClaudioLogin(w, http.StatusInternalServerError, loginPage{Kind: "error", Email: u.Email,
				Message: "Your account could not be prepared. Try again or contact an admin."})
			return
		}
		if ut.Status != usertoken.StatusActive {
			renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
				Message: "This account is disabled for claudio: account disabled, contact an admin."})
			return
		}
		if !s.logins.Approve(code, ut.ID) {
			renderClaudioLogin(w, http.StatusBadRequest, unknown)
			return
		}
		renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "done", Email: u.Email,
			Message: "Approved. Return to your terminal: claudio is signed in."})
	case "deny":
		if !s.logins.Deny(code) {
			renderClaudioLogin(w, http.StatusBadRequest, unknown)
			return
		}
		renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "done", Email: u.Email,
			Message: "Denied. Nothing was signed in."})
	default:
		renderClaudioLogin(w, http.StatusBadRequest, loginPage{Kind: "error", Email: u.Email, Message: "Bad request."})
	}
}

// memberToken returns the member's user token, creating it (name = e-mail,
// default limit) on their first login.
func (s *Server) memberToken(ctx context.Context, email string) (*usertoken.UserToken, error) {
	ut, err := usertoken.LookupByEmail(ctx, s.db, email)
	if !errors.Is(err, usertoken.ErrNotFound) {
		return ut, err
	}
	ut, err = usertoken.CreateForEmail(ctx, s.db, email)
	if errors.Is(err, usertoken.ErrEmailTaken) {
		// A concurrent approval created it first.
		return usertoken.LookupByEmail(ctx, s.db, email)
	}
	if err != nil {
		return nil, err
	}
	if l := s.defaultLimit; usertoken.HasLimit(l.OutputTokens, l.WindowSeconds) {
		if err := usertoken.SetLimit(ctx, s.db, ut.ID, l.OutputTokens, l.WindowSeconds); err != nil {
			// Never leave a member unlimited by accident.
			_ = usertoken.Delete(ctx, s.db, ut.ID)
			return nil, err
		}
		ut.LimitOutputTokens, ut.LimitWindowSeconds = l.OutputTokens, l.WindowSeconds
	}
	slog.Info("login: created user "+ut.Email, "id", ut.ID, "name", ut.Name)
	return ut, nil
}

// displayCode is a typed code as XXXX-XXXX (upper case, dashes and spaces
// dropped), or "" when it is not 8 letters and digits. The claudio API does
// the authoritative check.
func displayCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != 8 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return s[:4] + "-" + s[4:]
}

type loginPage struct {
	Kind    string // form | confirm | done | error
	Email   string
	Code    string // XXXX-XXXX, shown
	Hidden  string // the code as posted back
	Message string
}

func renderClaudioLogin(w http.ResponseWriter, status int, p loginPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := claudioLoginTmpl.Execute(w, p); err != nil {
		slog.Error("claudio login page", "err", err)
	}
}

var claudioLoginTmpl = template.Must(template.New("claudio-login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>claudio sign-in</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#f6f7f8;color:#1b1f23}
main{max-width:28rem;margin:4rem auto;padding:2rem;background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.12)}
h1{font-size:1.3rem;margin:0 0 1rem}
label{display:block;margin-bottom:.4rem}
input[name=code]{font:1.4rem ui-monospace,monospace;letter-spacing:.15em;text-transform:uppercase;width:100%;box-sizing:border-box;padding:.5rem}
button{font:inherit;padding:.5rem 1.2rem;margin:1rem .5rem 0 0;border:0;border-radius:6px;background:#1f6f5c;color:#fff;cursor:pointer}
button.deny{background:#6b7177}
.code{font:2.4rem ui-monospace,monospace;letter-spacing:.2em;text-align:center;margin:1rem 0}
.warn{background:#fff4d6;padding:.6rem .8rem;border-radius:6px}
.who{color:#5c636a;font-size:.9rem;margin-top:1.5rem}
</style>
</head>
<body>
<main>
<h1>claudio sign-in</h1>
{{if eq .Kind "form"}}
<form method="post" action="/claudio/login">
<input type="hidden" name="action" value="lookup">
<label for="code">Type the code shown in your terminal</label>
<input id="code" name="code" autocomplete="off" autocapitalize="characters" spellcheck="false" maxlength="16" required autofocus>
<button type="submit">Continue</button>
</form>
{{else if eq .Kind "confirm"}}
<p class="code">{{.Code}}</p>
<p class="warn">Approve only if this code is shown in your terminal.</p>
<p>claudio will be signed in as <strong>{{.Email}}</strong>.</p>
<form method="post" action="/claudio/login">
<input type="hidden" name="code" value="{{.Hidden}}">
<button type="submit" name="action" value="approve">Approve</button>
<button type="submit" name="action" value="deny" class="deny">Deny</button>
</form>
{{else}}
<p>{{.Message}}</p>
{{end}}
{{if .Email}}<p class="who">Signed in as {{.Email}}</p>{{end}}
</main>
</body>
</html>
`))
