package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"

	"cloud.google.com/go/datastore"
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
	imageWidth         = os.Getenv("IMAGE_WIDTH")
	imageHeight        = os.Getenv("IMAGE_HEIGHT")
)

var (
	psc  *pubsub.Client
	dsc  *datastore.Client
	gcsc *storage.Client
)

func init() {
	var err error

	psc, err = pubsub.NewClient(context.Background(), projectID)
	if err != nil {
		log.Fatal(err.Error())
	}

	dsc, err = datastore.NewClient(context.Background(), projectID)
	if err != nil {
		log.Fatal(err.Error())
	}

	gcsc, err = storage.NewClient(context.Background())
	if err != nil {
		log.Fatal(err.Error())
	}

	if imageWidth == "" {
		imageWidth = "512"
	}
	if imageHeight == "" {
		imageHeight = "512"
	}
}

type request struct {
	id string
	sc *slack.Client
	*pkg.Request
}

func main() {
	reqs := make(chan request)
	go func() {
		for {
			req := <-reqs
			sc := req.sc
			err := createImage(sc, req)
			if handleError(sc, err, req.GetChannelId(), req.GetTimestamp()) {
				continue
			}
			go func() {
				err = processImage(sc, req)
				handleError(sc, err, req.GetChannelId(), req.GetTimestamp())
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
			return
		}
		msg.Ack()

		sc, err := getSlackClient(req.GetUserId())
		if err != nil {
			log.Print(err.Error())
			return
		}
		log.Printf("%s msg received from %s: %s\n", id, getName(sc, req.GetUserId()), req.GetPrompt())
		err = updateMessage(sc, req.GetChannelId(), req.GetTimestamp(), "_Queueing..._")
		if err != nil {
			log.Print(err.Error())
		}

		reqs <- request{
			id:      id.String(),
			sc:      sc,
			Request: req,
		}
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}

type AuthedUser struct {
	UserID string
	Token  string
}

func getSlackClient(userID string) (*slack.Client, error) {
	user := new(AuthedUser)
	key := datastore.NameKey("UserToken", userID, nil)
	err := dsc.Get(context.Background(), key, user)
	if err != nil {
		return nil, err
	}
	client := slack.New(user.Token)
	return client, nil
}

func getName(sc *slack.Client, userID string) string {
	user, err := sc.GetUserInfo(userID)
	if err != nil {
		log.Print(err.Error())
		return "(error)"
	}
	return fmt.Sprintf("%s (%s)", user.Name, user.RealName)
}

func createImage(sc *slack.Client, req request) error {
	log.Printf("%s generating image\n", req.id)
	err := updateMessage(sc, req.GetChannelId(), req.GetTimestamp(), "_Generating..._")
	if err != nil {
		return err
	}

	args := []string{"--token", huggingfaceToken, "--uuid", req.id, "--W", imageWidth, "--H", imageHeight, "--prompt", req.GetPrompt()}
	cmd := exec.Command("./diffusion.py", args...)
	err = cmd.Run()
	if err != nil {
		return err
	}

	return nil
}

func processImage(sc *slack.Client, req request) error {
	log.Printf("%s uploading image\n", req.id)
	err := updateMessage(sc, req.GetChannelId(), req.GetTimestamp(), "_Loading..._")
	if err != nil {
		return err
	}

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
			"name":   getName(sc, req.GetUserId()),
		},
	})
	if err != nil {
		return err
	}

	log.Printf("%s sending response\n", req.id)

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

func updateMessage(sc *slack.Client, channel string, timestamp string, markdown string) error {
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", markdown, false, false),
				nil, nil,
			),
		),
	}

	_, _, _, err := sc.UpdateMessage(channel, timestamp, opts...)
	return err
}

var (
	errResponse = "Oh no! Something went wrong, give <@UU3TUL99S> a shout, hopefully he can get it working for you!"
)

func updateMessageError(sc *slack.Client, channel string, timestamp string) error {
	return updateMessage(sc, channel, timestamp, errResponse)
}

func handleError(sc *slack.Client, err error, channel string, timestamp string) bool {
	if err != nil {
		log.Print(err.Error())
		err = updateMessageError(sc, channel, timestamp)
		if err != nil {
			log.Print(err.Error())
		}
		return true
	}
	return false
}
