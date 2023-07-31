package gitkit

import (
	"fmt"
	"net/http"
)

type Credential struct {
	Username string
	Password string
	Token    string
}

func getCredential(req *http.Request) (Credential, error) {
	cred := Credential{}

	user, pass, ok := req.BasicAuth()
	if !ok {
		// return auth
		if token, ok := tokenAuth(req); ok {
			cred.Token = token
			return cred, nil
		}
		return cred, fmt.Errorf("authentication failed")
	}

	cred.Username = user
	cred.Password = pass

	return cred, nil
}

func tokenAuth(req *http.Request) (string, bool) {
	if token := req.Header.Get("Authorization"); token != "" {
		return token, true
	}
	return "", false
}
