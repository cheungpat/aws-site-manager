package main

import (
	"compress/gzip"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/awslabs/aws-sdk-go/aws"
	"github.com/awslabs/aws-sdk-go/service/cloudfront"
	"github.com/awslabs/aws-sdk-go/service/s3"
)

var contentTypeMap = map[string]string{
	"css":  "text/css",
	"html": "text/html",
	"htm":  "text/html",
	"ico":  "image/x-ico",
	"js":   "text/javascript",
	"jpg":  "image/jpeg",
	"gif":  "image/gif",
	"png":  "image/png",
	"xml":  "application/xml",
}

var compressBlacklist = map[string]bool{
	"gif":  true,
	"jpg":  true,
	"png":  true,
	"jpeg": true,
	"psd":  true,
	"ai":   true,
}

type FileInfo struct {
	path     string
	key      string
	fileInfo os.FileInfo
}

func Sync(bucket string, path string, reUpload bool, concurrentNum int) {
	svc := s3.New(&aws.Config{Region: defaultRegion})

	listObjOutput, err := svc.ListObjects(&s3.ListObjectsInput{
		Bucket: aws.String(bucket),
	})

	DisplayAwsErr(err)

	s3Keys := map[string]string{}
	updatedKeys := make([]*string, 0, 100)

	for _, s3Object := range listObjOutput.Contents {
		etag := *s3Object.ETag
		etag = etag[1 : len(etag)-1]
		s3Keys[*s3Object.Key] = etag
	}
	// TODO: Fix truncated result

	localFilesChan := make(chan *FileInfo, 100)
	doneChan := make(chan *string, 100)
	wg := sync.WaitGroup{}

	go GetAllFiles(path, localFilesChan)

	wg.Add(concurrentNum)
	for i := 0; i < concurrentNum; i++ {
		go UploadFileHandler(localFilesChan, &wg, bucket, &s3Keys, reUpload, doneChan)
	}

	go func() {
		for key := range doneChan {
			updatedKeys = append(updatedKeys, key)
		}
	}()
	wg.Wait()

	InvalidCloudFront(bucket, &updatedKeys)
}

func InvalidCloudFront(domain string, paths *[]*string) {
	if len(*paths) == 0 {
		return
	}

	distributionID := ""

	svc := cloudfront.New(nil)

	listDistInput := &cloudfront.ListDistributionsInput{
	// TODO: Marker: Handle truncated result
	}

	resp, err := svc.ListDistributions(listDistInput)

	for _, distribution := range resp.DistributionList.Items {
		for _, cname := range distribution.Aliases.Items {
			if *cname == domain {
				distributionID = *distribution.ID
			}
		}
		if distributionID != "" {
			break
		}
	}

	invalidationInput := &cloudfront.CreateInvalidationInput{
		DistributionID: aws.String(distributionID),
		InvalidationBatch: &cloudfront.InvalidationBatch{
			CallerReference: aws.String(GetCallerReference()),
			Paths: &cloudfront.Paths{
				Quantity: aws.Long(int64(len(*paths))),
				Items:    *paths,
			},
		},
	}

	fmt.Println("Send invalidate to Dist ID: " + distributionID)
	for _, key := range *paths {
		fmt.Println(*key)
	}
	_, err = svc.CreateInvalidation(invalidationInput)

	DisplayAwsErr(err)
}

func Hashfile(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		CheckErr(err)
	}
	defer file.Close()

	hasher := md5.New()
	io.Copy(hasher, file)
	hashVal := fmt.Sprintf("%x", hasher.Sum(nil))
	return hashVal, nil
}

func UploadFileHandler(localFilesChan chan *FileInfo, wg *sync.WaitGroup, bucket string, s3Keys *map[string]string, reUpload bool, doneChan chan *string) {
	svc := s3.New(&aws.Config{Region: defaultRegion})
	defer wg.Done()

	for file := range localFilesChan {
		uploadPath := file.path

		contentEncoding := ""
		suffix := strings.ToLower(filepath.Ext(file.path))[1:]
		if !compressBlacklist[suffix] && file.fileInfo.Size() > 500 {
			fmt.Println("Compressing: " + file.path)

			compressedFile, err := ioutil.TempFile("", "oursky")
			if err != nil {
				CheckErr(err)
			}
			gzipper, _ := gzip.NewWriterLevel(compressedFile, gzip.BestCompression)
			fileInput, err := os.Open(file.path)
			if err != nil {
				CheckErr(err)
			}
			io.Copy(gzipper, fileInput)
			fileInput.Close()
			gzipper.Close()
			uploadPath = compressedFile.Name()

			contentEncoding = "gzip"
		}
		contentType, ok := contentTypeMap[suffix]
		if !ok {
			contentType = "application/octet-stream"
			fmt.Println("Unknown ext: " + suffix)
		}

		hash, err := Hashfile(uploadPath)
		if err != nil {
			fmt.Println("Hash error: " + file.path)
		}

		etag, ok := (*s3Keys)[file.key]
		if ok && !reUpload && etag == hash {
			continue
		}

		fmt.Println("Uploading: " + uploadPath + " as " + file.key)
		fileIO, err := os.Open(uploadPath)

		paramsPutObject := &s3.PutObjectInput{
			Bucket:          aws.String(bucket),
			Key:             aws.String(file.key),
			Body:            fileIO,
			CacheControl:    aws.String("max-age=900"),
			ContentEncoding: aws.String(contentEncoding),
			ContentType:     aws.String(contentType),
			ACL:             aws.String("public-read"),
		}
		_, err = svc.PutObject(paramsPutObject)

		DisplayAwsErr(err)

		if err == nil {
			doneChan <- aws.String(EscapeURLForCF("/" + file.key))
		}
	}
}

func EscapeURLForCF(url string) string {
	re := regexp.MustCompile(" ")
	return re.ReplaceAllLiteralString(url, "%20")
}

func GetAllFiles(dirname string, localFilesChan chan *FileInfo) {
	var scan = func(path string, fileInfo os.FileInfo, err error) error {
		if path == "." {
			return nil
		}
		if !fileInfo.IsDir() && filepath.Base(path)[0] != '.' {
			key := strings.TrimPrefix(path, dirname)
			key = strings.TrimPrefix(key, "/")
			localFilesChan <- &FileInfo{path, key, fileInfo}
		}
		if fileInfo.IsDir() && filepath.Base(path)[0] == '.' {
			return filepath.SkipDir
		}
		return nil
	}

	filepath.Walk(dirname, scan)
	close(localFilesChan)
}