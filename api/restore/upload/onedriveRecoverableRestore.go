package upload

import (
	"encoding/json"
	"fmt"
	"log"
	"main/fileutil"
	http2 "main/graph/net/http"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	uploadSessionPath = "/users/%s/drive/root:/%s:/createUploadSession"
	uploadURLKey      = "uploadUrl"
)

func (rs *RestoreService) recoverableUpload(userID string, bearerToken string, conflictOption string, targetFolder string, filePath string, fileInfo fileutil.FileInfo, sendMsg func(text string), locText func(text string) string, username string) []map[string]interface{} {
	//1. Get recoverable upload session for the current file path 获取当前文件路径的可压缩上载会话
	uploadSessionData, err := rs.getUploadSession(userID, bearerToken, conflictOption, targetFolder, filePath)
	if err != nil {
		sendMsg(fmt.Sprintf(locText("filenameFail"), filePath))
		return nil
	}

	//2. Get the upload url returned as a response from the recoverable upload session above. 从上面的可压缩上载会话获取作为响应返回的上载url。
	uploadURL := uploadSessionData[uploadURLKey].(string)

	//3. Get the startOffset list for the file 获取文件的startOffset列表
	startOffsetLst, err := fileutil.GetFileOffsetStash(filePath)
	if err != nil {
		log.Panicf(locText("failToStore"), err)
	}
	_size, err := fileutil.GetFileSize(filePath)
	if err != nil {
		log.Panicf(locText("failToStore"), err)
	}
	//log.Panicln(fileutil.Byte2Readable(size))
	size := byte2Readable(float64(_size))

	//4. Loop over the file start offset list to read files in chunk and upload in onedrive 在文件开始偏移量列表上循环以读取块中的文件并在onedrive中上载
	var uploadResp []map[string]interface{}
	lastChunkIndex := len(startOffsetLst) - 1
	var isLastChunk bool
	timeUnix := time.Now().UnixNano()
	var buffer = make([]byte, fileutil.GetDefaultChunkSize())
	startTime := time.Now().Unix()

	for i, sOffset := range startOffsetLst {
		if i == lastChunkIndex {
			lastChunkSize, err := fileutil.GetLatsChunkSizeInBytes(filePath)
			lastChunkSize  = lastChunkSize + 1
			if err != nil {
				log.Panic(err)
			}
			buffer = make([]byte, lastChunkSize)
			isLastChunk = true
		}
		filePartInBytes := &buffer
		//4a. Get the bytes for the file based on the offset 根据偏移量获取文件的字节数
		err := fileutil.GetFilePartInBytes(filePartInBytes, filePath, sOffset)
		if err != nil {
			log.Panicf(locText("failToStore"), err)
		}
		if i != 0 {
			sendMsg(fmt.Sprintf(locText("oneDriveUploadTip1"), username, filePath, size, byte2Readable(float64(fileutil.GetDefaultChunkSize())*float64(i)), i, len(startOffsetLst), byte2Readable(float64(fileutil.GetDefaultChunkSize())/float64(time.Now().UnixNano()-timeUnix)*float64(1000000000)), time.Now().Unix()-startTime))
		} else {
			sendMsg(fmt.Sprintf(locText("oneDriveUploadTip2"), username, filePath, size, "0", i, len(startOffsetLst), time.Now().Unix()-startTime))
		}

		timeUnix = time.Now().UnixNano()
		//3b. make a call to the upload url with the file part based on the offset. 使用基于偏移量的文件部分调用上载url。
		var resp *http.Response
		for errCount := 1; errCount < 10; errCount++ {
			resp, err = rs.uploadFilePart(uploadURL, filePath, bearerToken, *filePartInBytes, sOffset, isLastChunk)
			if err != nil {
				bearerToken = http2.GetBearer() //解决长时上传时，Bearer超时的问题，这里采用超时一次就重新获取一次token的方案
				sendMsg("close|" + fmt.Sprintf(locText("failToLink"), username, filePath, errCount))
				// close 用作输出时定位，带有 close 在输出时不会被刷新走
				// close= 表示文件传输结束，此时会同步删除tg发出的消息
				// close| 则不会删除消息
			} else {
				break
			}
		}
		if err != nil {
			log.Fatalf("Failed to Load Files from source :%v", err)
		}

		respMap := make(map[string]interface{})
		err = json.NewDecoder(resp.Body).Decode(&respMap)
		if err != nil {
			fmt.Println(err)
		}
		if resp.Body != nil {
			defer resp.Body.Close()
		}
		//fmt.Printf("%+v, status code: %s", respMap, resp.Status)
		uploadResp = append(uploadResp, respMap)
		debug.FreeOSMemory()
	}

	sendMsg("close=" + fmt.Sprintf(fmt.Sprintf(locText("completeUpload"), filePath, time.Now().Unix()-startTime, byte2Readable(float64(_size)/float64(time.Now().UnixNano()-timeUnix)*float64(1000000000)))))
	return uploadResp
}

//Returns the restore session url for part file upload
func (rs *RestoreService) getUploadSession(userID string, bearerToken string, conflictOption string, targetFolder string, filePath string) (map[string]interface{}, error) {
	targetPath := strings.ReplaceAll(filepath.Join(targetFolder, filePath), "\\", "/")
	uploadSessionPath := fmt.Sprintf(uploadSessionPath, userID, targetPath)
	uploadSessionData := make(map[string]interface{})
	//Get the body for resemble upload session call.
	body, err := getRessumableSessionBody(filePath, conflictOption)
	if err != nil {
		return nil, err
	}
	//fmt.Printf("%+v", body)
	//log.Panicf("%+v", uploadSessionPath)

	//Create request instance
	req, err := rs.NewRequest("PUT", uploadSessionPath, getRessumableUploadSessionHeader(bearerToken), body)
	//log.Panicf("%+v", req)
	if err != nil {
		return nil, err
	}
	//Execute the request
	resp, err := rs.Do(req)
	// log.Panicf("%+v", resp)
	if err != nil {
		//Need to return a generic object from onedrive upload instead of response directly
		return nil, err
	}

	//convert http.Response to map
	err = json.NewDecoder(resp.Body).Decode(&uploadSessionData)
	if err != nil {
		return nil, err
	}
	return uploadSessionData, nil
}

//Uploads the file part to Onedrive
func (rs *RestoreService) uploadFilePart(uploadURL string, filePath string, bearerToken string, filePart []byte, startOffset int64, isLastPart bool) (*http.Response, error) {
	//This is required for Content-Range header key
	fileSizeInBytes, err := fileutil.GetFileSize(filePath)
	if err != nil {
		return nil, err
	}

	//Fetch Last chunklength -- will be needed in Content_length header
	lastChunkLength, err := fileutil.GetLatsChunkSizeInBytes(filePath)
	if err != nil {
		return nil, err
	}

	//Create upload part file request
	req, err := rs.NewRequest("PUT", uploadURL, getRessumableUploadHeader(fileSizeInBytes, bearerToken, startOffset, isLastPart, lastChunkLength), filePart)
	if err != nil {
		return nil, err
	}
	//Execute the request
	resp, err := rs.Do(req)
	if err != nil {
		//Need to return a generic object from onedrive upload instead of response directly
		return nil, err
	}
	return resp, nil
}

//Returns header for upload session API
func getRessumableUploadSessionHeader(accessToken string) map[string]string {
	//As a work around for now, ultimately this will be recived as a part of restore xml
	bearerToken := fmt.Sprintf("bearer %s", accessToken)
	return map[string]string{
		"Content-Type":  "application/json",
		"Authorization": bearerToken,
	}
}

//Returns headers for recoverable actual upload as file parts
func getRessumableUploadHeader(fileSizeInBytes int64, accessToken string, startOffset int64, isLastChunk bool, lastChunkSize int64) map[string]string {
	var cRange string
	var cLength string

	if isLastChunk {
		cRange = fmt.Sprintf("bytes %d-%d/%d", startOffset, fileSizeInBytes-1, fileSizeInBytes)
		cLength = fmt.Sprintf("%d", lastChunkSize+1)
	} else {
		cRange = fmt.Sprintf("bytes %d-%d/%d", startOffset, startOffset+fileutil.GetDefaultChunkSize()-1, fileSizeInBytes)
		cLength = fmt.Sprintf("%d", fileutil.GetDefaultChunkSize())
	}

	// fmt.Printf("\nCLength: %s , cRange: %s\n", cLength, cRange)
	bearerToken := fmt.Sprintf("bearer %s", accessToken)
	return map[string]string{
		"Content-Length": cLength,
		"Content-Range":  cRange,
		"Authorization":  bearerToken,
	}
}

//Returns the expected body for creating file upload session to onedrive
func getRessumableSessionBody(filePath string, conflictOption string) (string, error) {
	bodyMap := map[string]string{"@microsoft.graph.conflictBehavior": conflictOption, "description": "", "name": filePath}
	jsonBody, err := json.Marshal(bodyMap)
	return string(jsonBody), err
}

func byte2Readable(bytes float64) string {
	const kb float64 = 1024
	const mb float64 = kb * 1024
	const gb float64 = mb * 1024
	var readable float64
	var unit string
	_bytes := bytes

	if _bytes >= gb {
		// xx GB
		readable = _bytes / gb
		unit = "GB"
	} else if _bytes < gb && _bytes >= mb {
		// xx MB
		readable = _bytes / mb
		unit = "MB"
	} else {
		// xx KB
		readable = _bytes / kb
		unit = "KB"
	}
	return strconv.FormatFloat(readable, 'f', 2, 64) + " " + unit
}
