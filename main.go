package main

import (
    "bytes"
    "log"
    "fmt"
    "io"
    "os"
    "time"
    "strings"

    "crypto/md5"
    "encoding/hex"

    "encoding/json"
    "net/http"
    "archive/zip"

    "github.com/aws/aws-lambda-go/events"
    "github.com/aws/aws-lambda-go/lambda"

    "github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/session"
    "github.com/aws/aws-sdk-go/service/s3"
)

const (
    S3_REGION = "us-east-1"
    S3_BUCKET = "smushit"
)

type ApiRequest struct {
    Filename string `json:"filename"`
    Urls []interface{} `json:"urls"`
}

func main() {
    lambda.Start(HandleRequest)
}

func HandleRequest(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
    log.Printf("Request data: %v", request.Body)

    apiKey := request.Headers["X-Api-Key"]
    log.Printf("API Key: %v", apiKey)

    var apiRequestData = new(ApiRequest)
    _ = json.Unmarshal([]byte(request.Body), &apiRequestData)
    log.Printf("Parsed request data: %v", apiRequestData)

    if len(apiRequestData.Urls) <= 0 {
        return CreateErrorResponse("No URLs were provided")
    }

    var filesToArchive []string
    for _, url := range apiRequestData.Urls {
        fileUrl, _ := url.(string)
        filename, err := DownloadFromUrl(string(fileUrl))
        if err != nil {
            fmt.Println("Error while downloading file", filename, "-", err)
            continue
        }

        filesToArchive = append(filesToArchive, filename)
    }
    log.Printf("Files to archive: %v", filesToArchive)

    archiveFile := "/tmp/download.zip"
    ZipFiles(archiveFile, filesToArchive)

    // Create our AWS session
    sess, err := session.NewSession(&aws.Config{Region: aws.String(S3_REGION)})
    if err != nil {
        return CreateErrorResponse(err.Error())
    }

    if !strings.Contains(apiRequestData.Filename, ".zip") {
        apiRequestData.Filename += ".zip"
    }

    // Upload the file to S3
    filename := GetMD5Hash(apiKey) + "/" + apiRequestData.Filename
    err = AddFileToS3(sess, archiveFile, filename)
    if err != nil {
        return CreateErrorResponse(err.Error())
    }

    downloadUrl, err := GetDownloadUrl(sess, filename)
    if err != nil {
        return CreateErrorResponse(err.Error())
    }

    success := map[string]interface{}{
        "message": "Successfully uploaded " + apiRequestData.Filename,
        "downloadUrl": downloadUrl,
    }
    successResponse, _ := json.Marshal(success)
    return events.APIGatewayProxyResponse{
        Body: string(successResponse),
        StatusCode: 200,
    }, nil
}

func CreateErrorResponse(errorMessage string) (events.APIGatewayProxyResponse, error) {
    error := map[string]interface{}{"error": errorMessage}
    errorResponse, _ := json.Marshal(error)
    return events.APIGatewayProxyResponse{
        Body: string(errorResponse),
        StatusCode: 400,
    }, nil
}

func GetMD5Hash(text string) string {
    hasher := md5.New()
    hasher.Write([]byte(text))
    return hex.EncodeToString(hasher.Sum(nil))
}

func GetDownloadUrl(sess *session.Session, filename string) (string, error) {
    svc := s3.New(sess)

    req, _ := svc.GetObjectRequest(&s3.GetObjectInput{
        Bucket: aws.String(S3_BUCKET),
        Key: aws.String(filename),
    })
    urlStr, err := req.Presign(24 * time.Hour)
    return urlStr, err
}

// DownloadFromUrl will download a file at a given URL
func DownloadFromUrl(url string) (string, error) {
	filename := "/tmp/" + GetMD5Hash(url)
	fmt.Println("Downloading", url, "to", filename)

	// TODO: Check file existence first with io.IsExist
	output, err := os.Create(filename)
	if err != nil {
		return filename, err
	}
	defer output.Close()

	response, err := http.Get(url)
	if err != nil {
		return filename, err
	}
	defer response.Body.Close()

    // TODO: Add concurrency

	n, err := io.Copy(output, response.Body)
	if err != nil {
		return filename, err
	}

    // TODO: Check mime type for file extension

	fmt.Println(n, "bytes downloaded.")

    return filename, nil
}

// AddFileToS3 will upload a single file to S3
func AddFileToS3(sess *session.Session, fileToUpload string, filename string) error {
    log.Printf("Uploading file %s as %s", fileToUpload, filename)

    // Open the file for use
    file, err := os.Open(fileToUpload)
    if err != nil {
        return err
    }
    defer file.Close()

    // Get file size and read the file content into a buffer
    fileInfo, _ := file.Stat()
    var size int64 = fileInfo.Size()
    buffer := make([]byte, size)
    file.Read(buffer)

    // Config settings: this is where you choose the bucket, filename, content-type etc.
    // of the file you're uploading.
    _, err = s3.New(sess).PutObject(&s3.PutObjectInput{
        Bucket: aws.String(S3_BUCKET),
        Key: aws.String(filename),
        ACL: aws.String("private"),
        Body: bytes.NewReader(buffer),
        ContentLength: aws.Int64(size),
        ContentType: aws.String(http.DetectContentType(buffer)),
        ContentDisposition: aws.String("attachment"),
    })
    return err
}

// ZipFiles compresses one or many files into a single zip archive file
func ZipFiles(filename string, files []string) error {
    newfile, err := os.Create(filename)
    if err != nil {
        return err
    }
    defer newfile.Close()

    zipWriter := zip.NewWriter(newfile)
    defer zipWriter.Close()

    // Add files to zip
    for _, file := range files {
        zipfile, err := os.Open(file)
        if err != nil {
            return err
        }
        defer zipfile.Close()

        // Get the file information
        info, err := zipfile.Stat()
        if err != nil {
            return err
        }

        header, err := zip.FileInfoHeader(info)
        if err != nil {
            return err
        }

        // Change to deflate to gain better compression
        // see http://golang.org/pkg/archive/zip/#pkg-constants
        header.Method = zip.Deflate

        writer, err := zipWriter.CreateHeader(header)
        if err != nil {
            return err
        }
        _, err = io.Copy(writer, zipfile)
        if err != nil {
            return err
        }
    }
    return nil
}
