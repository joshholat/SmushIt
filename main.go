package main

import (
    "bytes"
    "log"
    "fmt"
    "io"
    "os"
    "time"
    "strings"
    "errors"

    "crypto/md5"
    "encoding/hex"

    "encoding/json"
    "net/http"
    "archive/zip"
    "path/filepath"

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

    // See https://medium.com/@dhanushgopinath/concurrent-http-downloads-using-go-32fecfa1ed27
    length := len(apiRequestData.Urls)
    done := make(chan string, length)
    errChan := make(chan error, length)
    for _, url := range apiRequestData.Urls {
        fileUrl, _ := url.(string)
        go func(url string) {
            filename, err := DownloadFromUrl(string(url))

            if err != nil {
                fmt.Println("Error while downloading file", filename, "-", err)
                errChan <- err
                done <- ""
                return
            }

            done <- filename
            errChan <- nil
        }(fileUrl)
    }
    for i := 0; i < length; i++ {
        downloadedFile := <-done
        if len(downloadedFile) > 0 {
            filesToArchive = append(filesToArchive, downloadedFile)
        }
    }
    log.Printf("%d files to archive: %v", len(filesToArchive), filesToArchive)

    if 0 == len(filesToArchive) {
        return CreateErrorResponse("No files were successfully downloaded")
    }

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
    extension := filepath.Ext(url)
    if len(extension) > 0 {
        filename += extension // Use the given extension
    }

    fmt.Println("Downloading", url, "to", filename)

    // TODO: Check file existence first with io.IsExist
    output, err := os.Create(filename)
    if err != nil {
        return filename, err
    }
    defer output.Close()

    req, err := http.NewRequest("GET", url, nil)
    if err != nil {
        return filename, err
    }
    // Here so sites see us like a "normal" browser
    req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux i686; rv:10.0) Gecko/20100101 Firefox/10.0")
    client := &http.Client{}
    response, err := client.Do(req)
    if err != nil {
        return filename, err
    }
    defer response.Body.Close()

    if response.StatusCode != http.StatusOK {
        return filename, errors.New("Invalid response (" + http.StatusText(response.StatusCode) + ")")
    }

    n, err := io.Copy(output, response.Body)
    if err != nil {
        return filename, err
    }
    fmt.Println(n, "bytes downloaded")

    file, err := os.Open(filename)
    if err != nil {
        return filename, err
    }
    defer file.Close()

    // Only the first 512 bytes are used to sniff the content type
    buffer := make([]byte, 512)
    _, err = file.Read(buffer)
    if err != nil {
        return filename, err
    }
    // Reset the read pointer if necessary
    file.Seek(0, 0)

    // Always returns a valid content-type and "application/octet-stream" if no others seemed to match.
    contentType := http.DetectContentType(buffer)
    if 0 == len(extension) {
        extensionFromMimeType := GetExtensionFromMimeType(contentType)
        if len(extensionFromMimeType) > 0 {
            filenameWithExtension := filename + "." + extensionFromMimeType
            err = os.Rename(filename, filenameWithExtension)
            if err != nil {
                return filename, err
            }
            filename = filenameWithExtension
        }
    }

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
    oneDayFromNow := time.Now().AddDate(0, 0, 1)
    _, err = s3.New(sess).PutObject(&s3.PutObjectInput{
        Bucket: aws.String(S3_BUCKET),
        Key: aws.String(filename),
        ACL: aws.String("private"),
        Body: bytes.NewReader(buffer),
        ContentLength: aws.Int64(size),
        ContentType: aws.String(http.DetectContentType(buffer)),
        ContentDisposition: aws.String("attachment"),
        Expires: aws.Time(oneDayFromNow), // The bucket policy handles this mainly
    })
    return err
}

func GetExtensionFromMimeType(mimeType string) string {
    mimeTypes := map[string]string{
        "image/png": "png",
        "image/jpeg": "jpeg",
        "image/jpg": "jpg",
        "image/gif": "gif",
        "audio/mpeg": "mp3",
        "video/mp4": "mp4",
    }

    if val, ok := mimeTypes[mimeType]; ok {
        return val
    }

    return ""
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
