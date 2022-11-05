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
	verifier, err := slack.NewSecretsVerifier(r.Header, slackSigningSecret)
	if handleErr(err, w) {
		return
	}

	r.Body = io.NopCloser(io.TeeReader(r.Body, &verifier))
	s, err := slack.SlashCommandParse(r)
	if handleErr(err, w) {
		return
	}

	err = verifier.Ensure()
	if handleErr(err, w) {
		return
	}

	switch s.Command {
	case "/diffusion":
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
		_, _, _, err = sc.SendMessage(s.ChannelID, opts...)
		if handleErr(err, w) {
			return
		}

		opts = []slack.MsgOption{
			slack.MsgOptionUser(s.UserID),
			slack.MsgOptionAsUser(true),
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn", "_Loading..._", false, false),
					nil, nil,
				),
			),
		}
		sc.Wait()
		channel, timestamp, _, err := sc.SendMessage(s.ChannelID, opts...)
		if handleErr(err, w) {
			return
		}

		req := &pkg.Request{
			Prompt:    s.Text,
			ChannelId: channel,
			Timestamp: timestamp,
		}
		bytes, err := proto.Marshal(req)
		if handleErr(err, w) {
			_, _, err = sc.DeleteMessage(channel, timestamp)
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
		if handleErr(err, w) {
			_, _, err = sc.DeleteMessage(channel, timestamp)
			if err != nil {
				log.Print(err.Error())
			}
			return
		}
	default:
		handleErr(fmt.Errorf("unknown command: %s", s.Command), w)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func handleErr(err error, w http.ResponseWriter) bool {
	if err != nil {
		log.Print(err.Error())
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("Oh no! Something went wrong, give <@UU3TUL99S> a shout, hopefully he can get it fixed for you!"))
		if err != nil {
			log.Print(err.Error())
		}
		return true
	}
	return false
}
