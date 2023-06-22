package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/slack-go/slack"
)

var (
	projectID          = os.Getenv("PROJECT_ID")
	pubsubTopic        = os.Getenv("PUBSUB_TOPIC")
	slackClientID      = os.Getenv("SLACK_CLIENT_ID")
	slackClientSecret  = os.Getenv("SLACK_CLIENT_SECRET")
	slackSigningSecret = os.Getenv("SLACK_SIGNING_SECRET")
)

type pubsubClient struct {
	*pubsub.Client
	sync.WaitGroup
}

type datastoreClient struct {
	*datastore.Client
	sync.WaitGroup
}

type storageClient struct {
	*storage.Client
	sync.WaitGroup
}

var (
	psc = new(pubsubClient)
	dsc = new(datastoreClient)
	gcs = new(storageClient)
)

// init sets up the Cloud Function endpoints, and sets up clients in a
// non-blocking manner
func init() {
	psc.Add(1)
	dsc.Add(1)
	gcs.Add(1)

	go func() {
		client, err := pubsub.NewClient(context.Background(), projectID)
		if err != nil {
			log.Fatal(err.Error())
		}
		psc.Client = client
		psc.Done()
	}()

	go func() {
		client, err := datastore.NewClient(context.Background(), projectID)
		if err != nil {
			log.Fatal(err.Error())
		}
		dsc.Client = client
		dsc.Done()
	}()

	go func() {
		client, err := storage.NewClient(context.Background())
		if err != nil {
			log.Fatal(err.Error())
		}
		gcs.Client = client
		gcs.Done()
	}()
}

// SlashFunction is the base handler for slash commands, will send back the
func SlashFunction(c *gin.Context) {
	verifier, err := slack.NewSecretsVerifier(c.Request.Header, slackSigningSecret)
	if handleError(err, c) {
		return
	}

	c.Request.Body = io.NopCloser(io.TeeReader(c.Request.Body, &verifier))
	s, err := slack.SlashCommandParse(c.Request)
	if handleError(err, c) {
		return
	}

	err = verifier.Ensure()
	if handleError(err, c) {
		return
	}

	if !userAuthed(s.UserID) {
		botScopes := []string{"commands"}
		userScopes := []string{"chat:write", "users:read"}
		authURL := fmt.Sprintf("https://slack.com/oauth/v2/authorize?client_id=%s&scope=%s&user_scope=%s", slackClientID, strings.Join(botScopes, ","), strings.Join(userScopes, ","))
		resp := "Oh no! It looks like you're not yet authorized, please follow the link below and try again!\n" + authURL
		c.String(http.StatusUnauthorized, resp)
		return
	}

	go func() {
		sc, err := getSlackClient(s.UserID)
		if handleErrorPost(err, s.ResponseURL) {
			return
		}

		opts := []slack.MsgOption{
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(
					slack.NewTextBlockObject("plain_text", s.Command+" "+s.Text, false, false),
					nil, nil,
				),
			),
		}

		// send back the users command as a message, allowing later messages to be sent before the http call returns
		_, _, _, err = sc.SendMessage(s.ChannelID, opts...)
		if handleErrorPost(err, s.ResponseURL) {
			return
		}

		handleRequestSlash(sc, s)
	}()
}

// handleRequestSlash routes the slash command to the relevant function
func handleRequestSlash(sc *slack.Client, s slack.SlashCommand) {
	switch s.Command {
	case "/diffusion":
		sendMessage(sc, s)
	default:
		handleErrorPost(fmt.Errorf("unknown command: %s", s.Command), s.ResponseURL)
		return
	}
}

type AuthedUser struct {
	UserID string
	Token  string
}

// userAuthed checks a user has a token stored in datastore
func userAuthed(userID string) bool {
	user := new(AuthedUser)
	key := datastore.NameKey("UserToken", userID, nil)
	dsc.Wait()
	err := dsc.Get(context.Background(), key, user)
	if err != nil {
		log.Print(err.Error())
		return false
	}
	return user.Token != ""
}

// getSlackClient will return a slack client using the token for the user stored
// in datastore
func getSlackClient(userID string) (*slack.Client, error) {
	user := new(AuthedUser)
	key := datastore.NameKey("UserToken", userID, nil)
	dsc.Wait()
	err := dsc.Get(context.Background(), key, user)
	if err != nil {
		return nil, err
	}
	client := slack.New(user.Token)
	return client, nil
}

type Request struct {
	Prompt    string
	ChannelId string
	Timestamp string
	UserId    string
}

// sendMessage creates a placeholder message used for status updates and the
// eventual image, and publishes the image request to pubsub
func sendMessage(sc *slack.Client, s slack.SlashCommand) {
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", "_Sending..._", false, false),
				nil, nil,
			),
		),
	}
	channel, timestamp, _, err := sc.SendMessage(s.ChannelID, opts...)
	if err != nil {
		log.Print(err.Error())
		return
	}

	req := Request{
		Prompt:    s.Text,
		ChannelId: channel,
		Timestamp: timestamp,
		UserId:    s.UserID,
	}

	err = GenerateFromPubSub(req)
	if err != nil {
		log.Print(err.Error())
		return
	}
}

var (
	errResponse = "Oh no! Something went wrong, give <@UU3TUL99S> a shout, hopefully he can get it working for you!"
)

func updateMessageError(client *slack.Client, channel string, timestamp string) error {
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", errResponse, false, false),
				nil, nil,
			),
		),
	}

	_, _, _, err := client.UpdateMessage(channel, timestamp, opts...)
	return err
}

// handleError will attempt to log an error if exists and write `errMessage` to
// http response, returns bool if error not nil
func handleError(err error, c *gin.Context) bool {
	if err != nil {
		log.Print(err.Error())
		c.String(http.StatusInternalServerError, errResponse)
		return true
	}
	return false
}

// handleErrorPost will attempt to log an error if exists and post `errMessage`
// to response URL, returns bool if error not nil
func handleErrorPost(err error, responseUrl string) bool {
	if err != nil {
		log.Print(err.Error())
		buf := bytes.NewBufferString(errResponse)
		_, err = http.Post(responseUrl, "text/plain", buf)
		if err != nil {
			log.Print(err.Error())
		}
		return true
	}
	return false
}

// AuthenticationFunction will handle the oauth2 redirect call for user auth,
// swapping code for token and storing in datastore
func AuthenticationFunction(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	code := values.Get("code")
	if code == "" {
		log.Print("no code provided")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp, err := slack.GetOAuthV2Response(http.DefaultClient, slackClientID, slackClientSecret, code, "")
	if err != nil {
		log.Print(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	user := &AuthedUser{
		UserID: resp.AuthedUser.ID,
		Token:  resp.AuthedUser.AccessToken,
	}
	key := datastore.NameKey("UserToken", user.UserID, nil)
	dsc.Wait()
	_, err = dsc.Put(context.Background(), key, user)
	if err != nil {
		log.Print(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain")
	_, err = w.Write([]byte("Authorized successfully! You can close this window now :)"))
	if err != nil {
		log.Print(err.Error())
	}
}
