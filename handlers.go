package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	//"github.com/jaekwon/go-prelude/colors"
)

//
// SERVE HTML, CSS, JS
//

func staticHandler(w http.ResponseWriter, r *http.Request) {
	var path string
	if strings.HasSuffix(r.URL.Path, "/") {
		path = r.URL.Path + "index.html"
	} else {
		path = r.URL.Path
	}
	http.ServeFile(w, r, "static/"+path)
}

//
// USER ROUTE
//

func userHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		publicKeyHandler(w, r)
	} else if r.Method == "POST" {
		createHandler(w, r)
	}
}

// GET /user/<public key hash> for public key lookup
// The server is untrusted, so the client will verify in Javascript
// that the public key we send here matches the hash they requested
func publicKeyHandler(w http.ResponseWriter, r *http.Request) {
	userPubHash := validateHash(r.URL.Path[len("/user/"):])
	userPub := LoadPubKey(userPubHash)
	if userPub == "" {
		http.Error(w, "Not found", http.StatusNotFound)
	} else {
		w.Write([]byte(userPub))
	}
}

// POST /publickeys/query to resolve addresses to public key hashes &
//  also look up public keys from those hashes.
// Read more here:
//  https://github.com/dcposch/scramble/wiki/Name-Resolution-&-Public-Key-Fetching
// The server is untrusted, so the client must verify everything.
func publicKeysHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	timestamp := time.Now().Unix()

	type PubKeyError struct {
		PubKey  string `json:"pubKey,omitempty"`
		PubHash string `json:"pubHash,omitempty"`
		Error   string `json:"error,omitempty"`
	}

	type Response struct {
		NameResolution map[string]*NotaryResultError `json:"nameResolution,omitempty"` // defined in notary.go
		PublicKeys     map[string]*PubKeyError       `json:"publicKeys,omitempty"`
	}

	type MxHostRespErr struct {
		MxHost string
		Resp   *http.Response
		Err    error
	}

	type PerMxHostRequest struct {
		NameAddresses EmailAddresses
		HashAddresses EmailAddresses
	}

	// parse addresses & group by mx host
	nameAddrs := ParseEmailAddresses(r.FormValue("nameAddresses"))
	mxHostNameAddrs, failedNameAddrs := GroupAddrsByMxHost(r.FormValue("nameAddresses"))
	mxHostHashAddrs, failedHostAddrs := GroupAddrsByMxHost(r.FormValue("hashAddresses"))
	notaries := ParseEmailAddresses(r.FormValue("notaries"))
	res := Response{}
	res.NameResolution = map[string]*NotaryResultError{}
	res.PublicKeys =     map[string]*PubKeyError{}

	// fail immediately if any address cannot be resolved.
	if (len(failedHostAddrs) != 0 || len(failedNameAddrs) != 0) {
		// TODO: better error handling
		failedHosts := []string{}
		for failedHost, _ := range failedHostAddrs {
			failedHosts = append(failedHosts, failedHost)
		}
		for failedHost, _ := range failedNameAddrs {
			failedHosts = append(failedHosts, failedHost)
		}
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("Host (%v) has no MX record", strings.Join(failedHosts, ","))))
		return
	}

	// server-to-server requests need no userId,
	// but all requested addresses should belong to this server.
	ourMxHost := GetConfig().SmtpMxHost
	if userId == nil {
		for host, _ := range mxHostHashAddrs {
			if host != ourMxHost {
				log.Panicf("Invalid host for server-to-server /publickeys/query request. Expected %v, got %v", ourMxHost, host)
			}
		}
		if len(notaries) > 1 || notaries[0].Host != GetConfig().SmtpMxHost {
			log.Panicf("Expected 0 or 1 notary address @"+GetConfig().SmtpMxHost+", got ["+notaries.String()+"]")
		}
	}

	// compile requests by mx host.
	allRequests := map[string]*PerMxHostRequest{}
	for mxHost, hashAddrs := range mxHostHashAddrs {
		allRequests[mxHost] = &PerMxHostRequest{nil, hashAddrs}
	}
	for _, notary := range notaries {
		if allRequests[notary.Host] == nil {
			allRequests[notary.Host] = &PerMxHostRequest{nameAddrs, nil}
		} else {
			allRequests[notary.Host].NameAddresses = nameAddrs
		}
	}

	// dispatch requests to each server.
	ch := make(chan *MxHostRespErr)
	counter := 0
	for mxHost, request := range allRequests {
		if mxHost == GetConfig().SmtpMxHost {
			// if host is this host

			// handle resolution request
			if len(request.NameAddresses) > 0 {
				signedResults := map[string]*NotarySignedResult{}
				thisResult := &NotaryResultError{signedResults, ""}
				for mxHost, addrs := range mxHostNameAddrs {
					for _, addr := range addrs {
						var pubHash string
						if mxHost == GetConfig().SmtpMxHost {
							// TODO: what if addr's host is not the user's default?
							pubHash = LoadPubHash(addr.Name)
						} else {
							pubHash = ResolveName(addr.Name, addr.Host)
						}
						// pubHash may be "", and that's a ok.
						signedResults[addr.String()] = &NotarySignedResult{
							pubHash,
							timestamp,
							SignNotaryResponse(addr.Name, addr.Host, pubHash, timestamp),
						}
					}
				}
				res.NameResolution[mxHost] = thisResult
			}

			// handle pubkey lookup
			for _, addr := range request.HashAddresses {
				pubHash := LoadPubHash(addr.Name)
				if pubHash == "" {
					res.PublicKeys[addr.StringNoHash()] = &PubKeyError{"", "", "Unknown name "+addr.Name}
					continue
				}
				pubKey := LoadPubKey(pubHash)
				if pubHash == addr.Hash {
					res.PublicKeys[addr.StringNoHash()] = &PubKeyError{pubKey, pubHash, ""}
				} else {
					res.PublicKeys[addr.StringNoHash()] = &PubKeyError{pubKey, pubHash, "Wrong hash for name"}
				}
			}
			// also load pubkeys for name addresses whose mx host is self
			for mxHost, addrs := range mxHostNameAddrs {
				if mxHost == GetConfig().SmtpMxHost {
					for _, addr := range addrs {
						pubHash := LoadPubHash(addr.Name)
						pubKey := LoadPubKey(pubHash)
						res.PublicKeys[addr.StringNoHash()] = &PubKeyError{pubKey, pubHash, ""}
					}
				}
			}

		} else {
			// if host is an external host
			counter += 1
			go func(mxHost string, request *PerMxHostRequest) {
				u := url.URL{}
				u.Scheme = "https"
				u.Host = mxHost
				u.Path = "/publickeys/query"
				body := url.Values{}
				body.Set("nameAddresses", request.NameAddresses.String())
				body.Set("hashAddresses", request.HashAddresses.String())
				body.Set("notaries", "notary@"+mxHost)
				resp, err := http.PostForm(u.String(), body)
				ch <- &MxHostRespErr{mxHost, resp, err}
			}(mxHost, request)
		}
	}

	// update `res` with responses
	timeout := time.After(5 * time.Second)
	timedOut := false
	for counter > 0 && !timedOut {
		select {
		case mxHostRespErr := <-ch:
			counter -= 1
			if mxHostRespErr.Err != nil {
				log.Printf("Error in /publickeys/query dispatch request: %s", mxHostRespErr.Err.Error())
				continue
			}
			respBody, err := ioutil.ReadAll(mxHostRespErr.Resp.Body)
			defer mxHostRespErr.Resp.Body.Close()
			if err != nil {
				log.Printf("Error in /publickeys/query dispatch request body read: %s", err.Error())
				continue
			}
			parsed := Response{}
			err = json.Unmarshal(respBody, &parsed)
			if err != nil {
				log.Println("Error in /publickeys/query json parse: %s", err.Error())
				continue
			}
			// aggregate notary responses
			res.NameResolution["notary@"+mxHostRespErr.MxHost] =
				parsed.NameResolution["notary@"+mxHostRespErr.MxHost]
			// merge pubkeys
			for addr, pubKeyErr := range parsed.PublicKeys {
				// The client still verifies the hash, but
				//  we should still be careful lest it tramples a valid entry
				//  from another host.
				if ParseEmailAddress(addr).Host != mxHostRespErr.MxHost {
					log.Printf("Dispatched /publickeys/query to %s returned public key for an invalid address: %s",
						mxHostRespErr.MxHost, addr)
				} else {
					res.PublicKeys[addr] = pubKeyErr
				}
			}
		case <-timeout:
			timedOut = true
		}
	}

	// fill remaining addresses with appropriate error messages
	// client must still verify that addresses aren't missing
	for mxHost, addrs := range mxHostNameAddrs {
		if mxHost == GetConfig().SmtpMxHost {
			continue // no need to fill error messages, already filled.
		}
		for _, addr := range addrs {
			if res.PublicKeys[addr.StringNoHash()] == nil {
				res.PublicKeys[addr.StringNoHash()] = &PubKeyError{"", "", "Failed to retrieve public key"}
			}
		}
	}
	for mxHost, addrs := range mxHostHashAddrs {
		if mxHost == GetConfig().SmtpMxHost {
			continue // no need to fill error messages, already filled.
		}
		for _, addr := range addrs {
			if res.PublicKeys[addr.StringNoHash()] == nil {
				res.PublicKeys[addr.StringNoHash()] = &PubKeyError{"", "", "Failed to retrieve public key"}
			}
		}
	}
	for _, notary := range notaries {
		if res.NameResolution[notary.StringNoHash()] == nil {
			res.NameResolution[notary.StringNoHash()] = &NotaryResultError{
				nil,
				fmt.Sprintf("Failed to retrieve notary response from %s", notary.String()),
			}
		}
	}

	// respond back
	resJson, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}
	w.Write(resJson)

	// drain ch
	for counter > 0 {
		select {
		case mxHostRespErr := <-ch:
			counter -= 1
			if mxHostRespErr.Err != nil {
				log.Printf("Error in (timed out) /publickeys/query dispatch request drain: %s", mxHostRespErr.Err.Error())
				continue
			}
			mxHostRespErr.Resp.Body.Close()
		}
	}
}

// POST /user to create a new account
// Remember that public and private key generation happens
// on the client. Public key, encrypted private key posted here.
func createHandler(w http.ResponseWriter, r *http.Request) {
	user := new(User)
	user.Token = validateToken(r.FormValue("token"))
	user.PasswordHash = validatePassHash(r.FormValue("passHash"))
	user.PublicKey = validatePublicKey(r.FormValue("publicKey"))
	user.PublicHash = ComputePublicHash(user.PublicKey)
	user.CipherPrivateKey = validateHex(r.FormValue("cipherPrivateKey"))
	user.EmailHost = computeEmailHost(r.Host)
	user.EmailAddress = user.Token+"@"+computeEmailHost(r.Host)

	log.Printf("Woot! New user %s %s\n", user.Token, user.PublicHash)

	if !SaveUser(user) {
		http.Error(w, "That username is taken", http.StatusBadRequest)
	}
}

// GET /user/me/contacts for the logged-in user's encrypted address book
// POST /user/me/contacts to update logged-in user's encrypted address book
// The entire address book is a single blob.
// Because the server never knows the plaintext, it is also
// unable to update individual keys in address book -- whenever
// the user makes changes, the client encrypts and posts all contacts
func contactsHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)

	if r.Method == "GET" {
		cipherContactsHex := LoadContacts(userId.Token)
		if cipherContactsHex == nil {
			http.Error(w, "Not found", http.StatusNotFound)
		} else {
			w.Write([]byte(*cipherContactsHex))
		}
	} else if r.Method == "POST" {
		cipherContactsHex, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		SaveContacts(userId.Token, string(cipherContactsHex))
	}
}

// GET /user/me/key for the logged-in user's encrypted private key
func privateKeyHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)

	user := LoadUser(userId.Token)
	if user == nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	w.Write([]byte(user.CipherPrivateKey))
}

//
// AUTHENTICATION
//

// Checks cookies, returns the logged-in user token or empty string
func authenticate(r *http.Request) *UserID {
	token, err := r.Cookie("token")
	if err != nil {
		return nil
	}
	passHash, err := r.Cookie("passHash")
	if err != nil {
		return nil
	}
	passHashOld, err := r.Cookie("passHashOld")
	var passHashOldVal string
	if err != nil {
		passHashOldVal = ""
	} else {
		passHashOldVal = passHashOld.Value
	}
	userId := authenticateUserPass(token.Value, passHash.Value, passHashOldVal)
	return userId
}

func computeEmailHost(requestHost string) string {
	if requestHost == "localhost" || strings.HasPrefix(requestHost, "localhost:") {
		return GetConfig().SmtpMxHost
	} else {
		if strings.Index(requestHost, ":") != -1 {
			host, _, err := net.SplitHostPort(requestHost)
			if err != nil {
				panic(err)
			}
			return host
		} else {
			return requestHost
		}
	}
}

func authenticateUserPass(token string, passHash string, passHashOld string) *UserID {
	// look up the user
	userId := LoadUserID(token)
	if userId == nil {
		return nil
	}

	// verify password
	if passHash == userId.PasswordHash && passHash != "" {
		return userId
	}
	if passHashOld == userId.PasswordHashOld && passHashOld != "" {
		return userId
	}
	return nil
}

//
// INBOX ROUTE
//

// Takes no arguments, returns all the metadata about a user's inbox.
// Encrypted subjects are returned, but no message bodies.
// The caller must have auth cookies set.
func inboxHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}
	box := r.URL.Path[len("/box/"):]

	var emailHeaders []EmailHeader
	if box == "inbox" || box == "archive" || box == "sent" {
		emailHeaders = LoadBox(userId.EmailAddress, box)
	} else {
		http.Error(w, "Unknown box. "+
			"Expected 'inbox','sent', etc, got "+box,
			http.StatusBadRequest)
		return
	}

	var inbox InboxSummary
	inbox.Token = userId.Token
	inbox.PublicHash = userId.PublicHash
	inbox.EmailHeaders = emailHeaders

	inboxJson, err := json.Marshal(inbox)
	if err != nil {
		panic(err)
	}
	w.Write(inboxJson)
}

//
// EMAIL ROUTE
//

func emailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		emailFetchHandler(w, r)
	} else if r.Method == "PUT" {
		emailBoxHandler(w, r)
	} else if r.Method == "POST" {
		emailSendHandler(w, r)
	}
}

// GET /email/id fetches the body
func emailFetchHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}

	id := r.URL.Path[len("/email/"):]
	validateMessageID(id)

	if len(BoxesForMessage(userId.EmailAddress, id)) > 0 {
		message := LoadMessage(id)
		w.Write([]byte(message.CipherBody))
	} else {
		http.Error(w, "Invalid message", http.StatusUnauthorized)
		return
	}
}

// PUT /email/id can change things about an email, eg what box it's in
func emailBoxHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}

	id := r.URL.Path[len("/email/"):]
	validateMessageID(id)
	newBox := validateBox(r.FormValue("box"))

	MoveEmail(userId.EmailAddress, id, newBox)
}

func emailSendHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}

	email := new(Email)
	email.MessageID = validateMessageID(r.FormValue("msgId"))
	email.UnixTime = time.Now().Unix()
	email.From = userId.EmailAddress
	email.To = r.FormValue("to")

	// XXX remove this case
	if r.FormValue("cipherBody") == "" { // unencrypted
		email.CipherSubject = r.FormValue("subject")
		email.CipherBody = r.FormValue("body")
	} else { // encrypted
		email.CipherSubject = validateHex(r.FormValue("cipherSubject"))
		email.CipherBody = validateHex(r.FormValue("cipherBody"))
	}

	// TODO: consider if transactions are required.
	// TODO: saveMessage may fail if messageId is not unique.
	SaveMessage(email)

	// add message to sender's sent box
	AddMessageToBox(email, userId.EmailAddress, "sent")

	// TODO: separate goroutine?
	// TODO: parallize mx lookup?

	// for each address, lookup MX record & determine what to do.
	mxHostAddrs, failedHostAddrs := GroupAddrsByMxHost(email.To)

	// fail immediately if any address cannot be resolved.
	if (len(failedHostAddrs) != 0) {
		// TODO: better error handling
		failedHosts := []string{}
		for failedHost, _ := range failedHostAddrs {
			failedHosts = append(failedHosts, failedHost)
		}
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("Destination host (%v) has no MX record", strings.Join(failedHosts, ","))))
		return
	}

	for mxHost, addrs := range mxHostAddrs {
		// if mxHost is GetConfig().SmtpMxHost, assume that the lookup will return itself.
		// this saves us from having to set up test MX records for localhost testing.
		if mxHost == GetConfig().SmtpMxHost {
			// add to inbox locally
			for _, addr := range addrs {
				AddMessageToBox(email, addr.String(), "inbox")
			}
			continue
		}

		// Add to outbox for delivery
		// Note that we only store the mxHost part for the 'address' column,
		//  and only once for multiple recipients on the same mxHost.
		// This is because SMTP can support sending the same email to
		//  multiple recipients on the same host at once.
		AddMessageToBox(email, mxHost, "outbox")
	}
}

//
// NGINX
//

// Tells where nginx should forward SMTP to
func nginxProxyHandler(w http.ResponseWriter, r *http.Request) {
	// http://nginx.org/en/docs/mail/ngx_mail_auth_http_module.html
	header := w.Header()
	header.Add("Auth-Status", "OK")
	header.Add("Auth-Server", "127.0.0.1")
	header.Add("Auth-Port", fmt.Sprintf("%d", GetConfig().SmtpPort))
	w.Write([]byte{})
}
