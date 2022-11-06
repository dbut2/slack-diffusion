package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/dbut2/slack-diffusion/proto/pkg"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"google.golang.org/protobuf/proto"
)

var (
	huggingfaceToken   = os.Getenv("HUGGINGFACE_TOKEN")
	projectID          = os.Getenv("PROJECT_ID")
	pubsubSubscription = os.Getenv("PUBSUB_SUBSCRIPTION")
	storageBucket      = os.Getenv("STORAGE_BUCKET")
	slackToken         = os.Getenv("SLACK_TOKEN")
	imageWidth         = os.Getenv("IMAGE_WIDTH")
	imageHeight        = os.Getenv("IMAGE_HEIGHT")
)

var (
	psc  *pubsub.Client
	gcsc *storage.Client
	sc   *slack.Client
)

func init() {
	var err error

	psc, err = pubsub.NewClient(context.Background(), projectID)
	if err != nil {
		log.Fatal(err.Error())
	}

	gcsc, err = storage.NewClient(context.Background())
	if err != nil {
		log.Fatal(err.Error())
	}

	sc = slack.New(slackToken)

	if imageWidth == "" {
		imageWidth = "512"
	}
	if imageHeight == "" {
		imageHeight = "512"
	}
}

type request struct {
	id string
	*pkg.Request
}

func main() {
	reqs := make(chan request)
	go func() {
		for {
			req := <-reqs
			err := createImage(req)
			if handleError(err, req.GetChannelId(), req.GetTimestamp()) {
				continue
			}
			go func() {
				err = processImage(req)
				handleError(err, req.GetChannelId(), req.GetTimestamp())
			}()
		}
	}()

	sub := psc.Subscription(pubsubSubscription)
	log.Print("listening...\n")
	err := sub.Receive(context.Background(), func(ctx context.Context, msg *pubsub.Message) {
		id := uuid.New()

		req := new(pkg.Request)
		err := proto.Unmarshal(msg.Data, req)
		if err != nil {
			msg.Nack()
			log.Print(err.Error())
		}

		msg.Ack()

		fmt.Printf("%s msg received from %s: %s\n", id, getName(req.GetUserId()), req.GetPrompt())

		reqs <- request{
			id:      id.String(),
			Request: req,
		}
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}

func getName(userID string) string {
	user, err := sc.GetUserInfo(userID)
	if err != nil {
		log.Print(err.Error())
		return "(error)"
	}
	return fmt.Sprintf("%s (%s)", user.Name, user.RealName)
}

func createImage(req request) error {
	fmt.Printf("%s generating image\n", req.id)

	args := []string{"--token", huggingfaceToken, "--uuid", req.id, "--W", imageWidth, "--H", imageHeight, "--prompt", req.GetPrompt()}
	cmd := exec.Command("./diffusion.py", args...)
	err := cmd.Run()
	if err != nil {
		return err
	}

	fmt.Printf("%s saving image\n", req.id)

	return nil
}

func processImage(req request) error {
	fmt.Printf("%s uploading image\n", req.id)

	filename := fmt.Sprintf("%s.png", req.id)

	obj := gcsc.Bucket(storageBucket).Object(filename)
	w := obj.NewWriter(context.Background())

	file, err := os.Open("output/" + filename)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, file)
	if err != nil {
		return err
	}
	err = file.Close()
	if err != nil {
		return err
	}

	err = w.Close()
	if err != nil {
		return err
	}

	err = os.Remove("output/" + filename)
	if err != nil {
		return err
	}

	_, err = obj.Update(context.Background(), storage.ObjectAttrsToUpdate{
		Metadata: map[string]string{
			"prompt": req.GetPrompt(),
			"userId": req.GetUserId(),
			"name":   getName(req.GetUserId()),
		},
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s sending response\n", req.id)

	imageUrl := fmt.Sprintf("https://storage.googleapis.com/%s/%s", obj.BucketName(), obj.ObjectName())

	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewImageBlock(imageUrl, req.GetPrompt(), req.id, slack.NewTextBlockObject("plain_text", req.GetPrompt(), false, false)),
		),
	}

	_, _, _, err = sc.UpdateMessage(req.GetChannelId(), req.GetTimestamp(), opts...)
	if err != nil {
		return err
	}

	return nil
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

func handleError(err error, channel string, timestamp string) bool {
	if err != nil {
		log.Print(err.Error())
		err = updateMessageError(channel, timestamp)
		if err != nil {
			log.Print(err.Error())
		}
		return true
	}
	return false
}
