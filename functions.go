package functions

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"cloud.google.com/go/pubsub"
	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/slack-go/slack"
	"google.golang.org/protobuf/proto"

	"github.com/dbut2/slack-diffusion/proto/pkg"
)

var (
	projectID          = os.Getenv("PROJECT_ID")
	pubsubTopic        = os.Getenv("PUBSUB_TOPIC")
	slackToken         = os.Getenv("SLACK_TOKEN")
	slackSigningSecret = os.Getenv("SLACK_SIGNING_SECRET")
)

type pubsubClient struct {
	*pubsub.Client
	sync.WaitGroup
}

type slackClient struct {
	*slack.Client
	sync.WaitGroup
}

var (
	psc = new(pubsubClient)
	sc  = new(slackClient)
)

func init() {
	functions.HTTP("DiffusionRequest", requestFunc)

	psc.Add(1)
	sc.Add(1)

	go func() {
		client, err := pubsub.NewClient(context.Background(), projectID)
		if err != nil {
			panic(err.Error())
		}
		psc.Client = client
		psc.Done()
	}()

	go func() {
		client := slack.New(slackToken)
		sc.Client = client
		sc.Done()
	}()
}

func requestFunc(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)

	verifier, err := slack.NewSecretsVerifier(r.Header, slackSigningSecret)
	if handleError(err, w) {
		return
	}

	r.Body = io.NopCloser(io.TeeReader(r.Body, &verifier))
	s, err := slack.SlashCommandParse(r)
	if handleError(err, w) {
		return
	}

	err = verifier.Ensure()
	if handleError(err, w) {
		return
	}

	switch s.Command {
	case "/diffusion":
		go sendMessage(s)
	default:
		handleError(fmt.Errorf("unknown command: %s", s.Command), w)
		return
	}
}

func sendMessage(s slack.SlashCommand) {
	opts := []slack.MsgOption{
		slack.MsgOptionUser(s.UserID),
		slack.MsgOptionAsUser(true),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("plain_text", "/diffusion "+s.Text, false, false),
				nil, nil,
			),
		),
	}
	sc.Wait()
	_, _, _, err := sc.SendMessage(s.ChannelID, opts...)
	if err != nil {
		log.Print(err.Error())
		return
	}

	opts = []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", "_Loading..._", false, false),
				nil, nil,
			),
		),
	}
	sc.Wait()
	channel, timestamp, _, err := sc.SendMessage(s.ChannelID, opts...)
	if err != nil {
		log.Print(err.Error())
		return
	}

	req := &pkg.Request{
		Prompt:    s.Text,
		ChannelId: channel,
		Timestamp: timestamp,
		UserId:    s.UserID,
	}
	bytes, err := proto.Marshal(req)
	if err != nil {
		log.Print(err.Error())
		err = updateMessageError(channel, timestamp)
		if err != nil {
			log.Print(err.Error())
		}
		return
	}
	msg := &pubsub.Message{
		Data: bytes,
	}

	psc.Wait()
	res := psc.Topic(pubsubTopic).Publish(context.Background(), msg)
	_, err = res.Get(context.Background())
	if err != nil {
		log.Print(err.Error())
		err = updateMessageError(channel, timestamp)
		if err != nil {
			log.Print(err.Error())
		}
		return
	}
}

var (
	errResponse = "Oh no! Something went wrong, give <@UU3TUL99S> a shout, hopefully he can get it working for you!"
)

func updateMessageError(channel string, timestamp string) error {
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", errResponse, false, false),
				nil, nil,
			),
		),
	}

	_, _, _, err := sc.UpdateMessage(channel, timestamp, opts...)
	return err
}

func handleError(err error, w http.ResponseWriter) bool {
	if err != nil {
		log.Print(err.Error())
		_, err = w.Write([]byte(errResponse))
		if err != nil {
			log.Print(err.Error())
		}
		return true
	}
	return false
}
