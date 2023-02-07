// Package p contains a Pub/Sub Cloud Function.
package p

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"google.golang.org/protobuf/proto"

	"github.com/dbut2/slack-diffusion/proto/pkg"
)

// PubSubMessage is the payload of a Pub/Sub event. Please refer to the docs for
// additional information regarding Pub/Sub events.
type PubSubMessage struct {
	Data []byte `json:"data"`
}

func GenerateFromPubSub(ctx context.Context, m PubSubMessage) error {
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		return errors.New("PROJECT_ID is blank")
	}

	dsc, err := datastore.NewClient(ctx, projectID)
	if err != nil {
		return err
	}

	gcsc, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}

	id := uuid.New()

	req := new(pkg.Request)
	err = proto.Unmarshal(m.Data, req)
	if err != nil {
		return err
	}

	sc, err := getSlackClient(dsc, req.GetUserId())
	if err != nil {
		return err
	}

	log.Printf("%s msg received from %s: %s\n", id, getName(sc, req.GetUserId()), req.GetPrompt())

	err = updateMessage(sc, req.GetChannelId(), req.GetTimestamp(), "_Generating..._")
	if err != nil {
		return err
	}

	images, err := callApi(req.GetPrompt())
	if err != nil {
		updateMessageError(sc, req.GetChannelId(), req.GetTimestamp())
		return err
	}

	err = updateMessage(sc, req.GetChannelId(), req.GetTimestamp(), "_Loading..._")
	if err != nil {
		return err
	}

	bucket := os.Getenv("STORAGE_BUCKET")
	imageUrls, err := writeBytes(gcsc, bucket, id.String(), images)
	if err != nil {
		updateMessageError(sc, req.GetChannelId(), req.GetTimestamp())
		return err
	}

	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewImageBlock(imageUrls[0], req.GetPrompt(), "blockID", slack.NewTextBlockObject("plain_text", req.GetPrompt(), false, false)),
		),
	}

	_, _, _, err = sc.UpdateMessage(req.GetChannelId(), req.GetTimestamp(), opts...)
	if err != nil {
		return err
	}

	return nil
}

type AuthedUser struct {
	UserID string
	Token  string
}

// getSlackClient will return a slack client using the token for the user stored
// in datastore
func getSlackClient(dsc *datastore.Client, userID string) (*slack.Client, error) {
	user := new(AuthedUser)
	key := datastore.NameKey("UserToken", userID, nil)
	err := dsc.Get(context.Background(), key, user)
	if err != nil {
		return nil, err
	}
	client := slack.New(user.Token)
	return client, nil
}

// getName will fetch the users name from slack
func getName(sc *slack.Client, userID string) string {
	user, err := sc.GetUserInfo(userID)
	if err != nil {
		log.Print(err.Error())
		return "(error)"
	}
	return fmt.Sprintf("%s (%s)", user.Name, user.RealName)
}

// updateMessage is utility func to change text of placeholder message
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

// updateMessageError sets the placeholder message to generic error
func updateMessageError(sc *slack.Client, channel string, timestamp string) error {
	return updateMessage(sc, channel, timestamp, errResponse)
}

type TextToImageImage struct {
	Base64       string `json:"base64"`
	Seed         uint32 `json:"seed"`
	FinishReason string `json:"finishReason"`
}

type TextToImageResponse struct {
	Images []TextToImageImage `json:"artifacts"`
}

func callApi(prompt string) ([][]byte, error) {
	engineId := "stable-diffusion-512-v2-0"
	apiHost := os.Getenv("STABILITY_API_HOST")
	if apiHost == "" {
		apiHost = "https://api.stability.ai"
	}

	reqUrl := apiHost + "/v1beta/generation/" + engineId + "/text-to-image"

	apiKey := os.Getenv("STABILITY_API_KEY")
	if apiKey == "" {
		return nil, errors.New("STABILITY_API_KEY is blank")
	}

	height := os.Getenv("IMAGE_HEIGHT")
	width := os.Getenv("IMAGE_WIDTH")

	var data = []byte(fmt.Sprintf(`{
		"text_prompts": [
		  {
			"text": "%s"
		  }
		],
		"cfg_scale": 7,
		"clip_guidance_preset": "FAST_BLUE",
		"height": %s,
		"width": %s,
		"samples": 1,
		"steps": 50
  	}`, prompt, height, width))

	req, _ := http.NewRequest("POST", reqUrl, bytes.NewBuffer(data))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+apiKey)

	// Execute the request & read all the bytes of the body
	res, err := http.DefaultClient.Do(req)
	defer res.Body.Close()
	if err != nil {
		return nil, err
	}

	if res.StatusCode != 200 {
		var body map[string]interface{}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			return nil, err
		}
		return nil, errors.New(fmt.Sprintf("status != 200: %s", body))
	}

	var body TextToImageResponse
	if err = json.NewDecoder(res.Body).Decode(&body); err != nil {
		panic(err)
	}

	var images [][]byte

	for _, image := range body.Images {
		imageBytes, err := base64.StdEncoding.DecodeString(image.Base64)
		if err != nil {
			return nil, err
		}
		images = append(images, imageBytes)
	}

	return images, nil
}

func writeBytes(gcsc *storage.Client, bucket string, id string, images [][]byte) ([]string, error) {
	var imageUrls []string

	for i, image := range images {
		filename := fmt.Sprintf("%s_%d.png", id, i)

		obj := gcsc.Bucket(bucket).Object(filename)
		w := obj.NewWriter(context.Background())

		_, err := w.Write(image)
		if err != nil {
			return nil, err
		}

		err = w.Close()
		if err != nil {
			return nil, err
		}

		imageUrl := fmt.Sprintf("https://storage.googleapis.com/%s/%s", obj.BucketName(), obj.ObjectName())
		imageUrls = append(imageUrls, imageUrl)
	}

	return imageUrls, nil
}
