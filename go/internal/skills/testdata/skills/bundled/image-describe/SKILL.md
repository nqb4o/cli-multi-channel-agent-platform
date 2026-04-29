---
name: image-describe
version: 1.0.0
description: Describe the content of an image
when_to_use: When the user provides an image and wants a description
mcp:
  enabled: true
  command: [python, -m, image_describe_server]
  transport: stdio
signing:
  publisher: openclaw-official
  sig: fakesig123
---
# Image Describe

Uses vision capabilities to describe image content in detail.
