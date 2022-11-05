FROM golang AS builder

WORKDIR /app

COPY ./vendor ./vendor
COPY ./go.mod ./go.mod
COPY ./go.sum ./go.sum
COPY ./proto/pkg ./proto/pkg

COPY main.go ./main.go

RUN go build -o diffusion main.go

FROM tensorflow/tensorflow:2.10.0-gpu

RUN rm -rf /usr/local/cuda/lib64/stubs

COPY requirements.txt /

RUN pip install -r requirements.txt \
  --extra-index-url https://download.pytorch.org/whl/cu117

RUN useradd -m huggingface

USER huggingface

WORKDIR /home/huggingface

ENV USE_TORCH=1

RUN mkdir -p /home/huggingface/.cache/huggingface \
  && mkdir -p /home/huggingface/output

COPY diffusion.py ./
COPY token.txt /home/huggingface

COPY --from=builder /app/diffusion ./

CMD ["./diffusion"]
