### drive-untrash

## Install

`go get -u gitlab.com/B4dM4n/drive-untrash`

## Setup

Follow the steps on https://developers.google.com/drive/v3/web/quickstart/go 
to get the `client_secret.json` file

## Usage

```
drive-untrash [folderID]...
  -v	verbose logging
```

Without folderID's specified, all trashed files in Google Drive will get restored.