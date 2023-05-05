package in

import "github.com/alphagov/paas-s3-resource"

type Request struct {
	Source  s3resource.Source  `json:"source"`
	Version s3resource.Version `json:"version"`
	Params  Params             `json:"params"`
}

type Params struct {
	Unpack       bool   `json:"unpack"`
	DownloadTags bool   `json:"download_tags"`
	SkipDownload string `json:"skip_download"`
}

type Response struct {
	Version  s3resource.Version        `json:"version"`
	Metadata []s3resource.MetadataPair `json:"metadata"`
}
