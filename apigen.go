package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

func GenerateFromPubSub(req Request) error {
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		return errors.New("PROJECT_ID is blank")
	}

	id := uuid.New()

	sc, err := getSlackClient(req.UserId)
	if err != nil {
		return err
	}

	log.Printf("%s msg received from %s: %s", id, getName(sc, req.UserId), req.Prompt)

	prompt, count, err := parsePrompt(req.Prompt)
	if err != nil {
		return err
	}

	if count < 1 {
		count = 1
	}

	maxImages := 4
	if count > maxImages {
		log.Printf("%s too many images requested: %d", id, count)
		count = maxImages
	}

	err = updateMessage(sc, req.ChannelId, req.Timestamp, "_Generating..._")
	if err != nil {
		return err
	}

	images, err := callApi(prompt, count)
	if err != nil {
		updateMessageError(sc, req.ChannelId, req.Timestamp)
		return err
	}

	err = updateMessage(sc, req.ChannelId, req.Timestamp, "_Loading..._")
	if err != nil {
		updateMessageError(sc, req.ChannelId, req.Timestamp)
		return err
	}

	bucket := os.Getenv("STORAGE_BUCKET")
	imageUrls, err := writeBytes(gcs.Client, bucket, id.String(), images)
	if err != nil {
		updateMessageError(sc, req.ChannelId, req.Timestamp)
		return err
	}

	var blocks []slack.Block
	for i, image := range imageUrls {
		blocks = append(blocks, slack.NewImageBlock(image, prompt, fmt.Sprintf("image_%d", i), slack.NewTextBlockObject("plain_text", prompt, false, false)))
	}

	_, _, _, err = sc.UpdateMessage(req.ChannelId, req.Timestamp, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		updateMessageError(sc, req.ChannelId, req.Timestamp)
		return err
	}

	return nil
}

func parsePrompt(prompt string) (string, int, error) {
	var p string
	var c int

	r := regexp.MustCompile("^(x(\\d*) )?(.*)$")
	res := r.FindAllStringSubmatch(prompt, -1)

	var err error

	if str := res[0][2]; str != "" {
		c, err = strconv.Atoi(res[0][2])
		if err != nil {
			return "", 0, err
		}
	}

	p = res[0][3]

	return p, c, nil
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

type TextToImageImage struct {
	Base64       string `json:"base64"`
	Seed         uint32 `json:"seed"`
	FinishReason string `json:"finishReason"`
}

type TextToImageResponse struct {
	Images []TextToImageImage `json:"artifacts"`
}

func callApi(prompt string, count int) ([][]byte, error) {
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
		"samples": %d,
		"steps": 50
  	}`, prompt, height, width, count))

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

	log.Print(req)
	log.Print(res)

	if res.StatusCode != 200 {
		var body map[string]interface{}
		buf := &bytes.Buffer{}
		res.Body = io.NopCloser(io.TeeReader(res.Body, buf))
		log.Print(buf.String())
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			return nil, err
		}
		return nil, errors.New(fmt.Sprintf("status != 200: %d, %s", res.StatusCode, body))
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
