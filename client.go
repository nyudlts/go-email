package go_email

import (
	"bufio"
	b64 "encoding/base64"
	"fmt"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"log"
	"regexp"
	"strings"
)

var plaintext = regexp.MustCompile("text/plain")

type EmailAccount struct {
	Account  string `yaml:"account"`
	Password string `yaml:"password"`
	Server   string `yaml:"server"`
	Port     string `yaml:"port"`
}

type Email struct {
	Message *imap.Message
	Section *imap.BodySectionName
}

func GetCreds(account string) (EmailAccount, error) {
	credsMap := map[string]EmailAccount{}
	source, err := ioutil.ReadFile("/etc/go-email.yml")
	if err != nil {
		return EmailAccount{}, err
	}

	err = yaml.Unmarshal(source, &credsMap)
	if err != nil {
		return EmailAccount{}, err
	}

	for k, v := range credsMap {
		if account == k {
			return v, nil
		}
	}

	return EmailAccount{}, fmt.Errorf("Credentials file did not contain %s\n", account)
}

func GetClient(accountName string) (*client.Client, error) {
	log.Println("Connecting to server...")

	account, err := GetCreds(accountName)
	if err != nil {
		return nil, err
	}

	// Connect to server
	addr := fmt.Sprintf(account.Server + ":" + account.Port)
	c, err := client.DialTLS(addr, nil)
	if err != nil {
		return c, err
	}
	log.Println("Connected")
	return c, nil
}

func GetMailboxes(c *client.Client) ([]string, error) {
	mailboxes := []string{}
	mailboxChannel := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxChannel)
	}()

	log.Println("Mailboxes:")
	for m := range mailboxChannel {
		mailboxes = append(mailboxes, m.Name)
	}

	if err := <-done; err != nil {
		return mailboxes, err
	}

	return mailboxes, nil
}

func GetMailbox(c *client.Client, name string) (*imap.MailboxStatus, error) {
	mailbox, err := c.Select(name, true)
	if err != nil {
		return mailbox, err
	}
	return mailbox, nil
}

func GetMessage(i uint32, c *client.Client) (Email, error) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(i)
	done := make(chan error, 1)

	var section imap.BodySectionName
	items := []imap.FetchItem{section.FetchItem()}

	messages := make(chan *imap.Message, 1)

	go func() {
		done <- c.Fetch(seqset, items, messages)
	}()

	if err := <-done; err != nil {
		return Email{}, nil
	}

	msg := <-messages

	return Email{msg, &section}, nil
}

func GetMessages(from uint32, to uint32, c *client.Client) ([]Email, error) {

	emails := []Email{}
	for i := to; i >= from; i-- {
		email, err := GetMessage(i, c)
		if err != nil {
			return emails, err
		}
		emails = append(emails, email)
	}

	return emails, nil
}

func WriteMessage(writer *bufio.Writer, email Email) error {
	r := email.Message.GetBody(email.Section)
	mr, err := mail.CreateReader(r)
	if err != nil {
		return err
	}

	writer.WriteString(fmt.Sprintf("From %s %s\r\n", mr.Header.Get("from"), mr.Header.Get("date")))
	writer.Flush()

	//Message Headers
	boundaryCode, err := writeMessageHeaders(mr.Header.Fields(), writer)
	if err != nil {
		return err
	}

	for {

		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		switch h := p.Header.(type) {
		//handle inline headers
		case *mail.InlineHeader:
			hf := h.Fields()
			writePartHeaders(boundaryCode, hf, writer)
			b, err := ioutil.ReadAll(p.Body)
			if err != nil {
				return err
			}
			writeBody(string(b), writer)
			writer.Flush()
		//handle attachments
		case *mail.AttachmentHeader:
			writePartHeaders(boundaryCode, h.Fields(), writer)

			b, err := ioutil.ReadAll(p.Body)
			if err != nil {
				return err
			}

			if p.Header.Get("Content-transfer-Encoding") == "base64" {
				writeBase64EncodedPart(b, writer)
			}
		}
	}

	return nil
}

func writeMessageHeaders(fields message.HeaderFields, writer *bufio.Writer) (string, error) {

	var boundaryCode string

	for fields.Next() {

		values := fields.Value()
		bc := CheckBoundary(fields.Value())
		if bc != "" {
			boundaryCode = bc
		}

		formattedValues := getSubValues(values)
		writer.WriteString(fmt.Sprintf("%s: %v\r\n", fields.Key(), formattedValues))
		writer.Flush()

	}

	return boundaryCode, nil
}

func writePartHeaders(bc string, hf message.HeaderFields, writer *bufio.Writer) {

	writer.WriteString("\r\n" + bc + "\r\n")
	writer.Flush()
	for hf.Next() {
		value := strings.ReplaceAll(hf.Value(), ";", ";\r\n ")
		writer.WriteString(fmt.Sprintf("%s: %s\r\n", hf.Key(), value))
		writer.Flush()
	}
	writer.WriteString("\r\n")
	writer.Flush()
}

func writeBody(s string, writer *bufio.Writer) {
	lines := strings.Split(s, "\r\n")
	for _, line := range lines {
		if len(line) > 75 {
			for _, c := range chunk(line, 75) {
				writer.WriteString(c + "=\r\n")
			}
		} else {
			writer.WriteString(line + "\r\n")
		}
	}
}

func chunk(s string, chunkSize int) []string {
	if chunkSize >= len(s) {
		return []string{s}
	}
	var chunks []string
	chunk := make([]rune, chunkSize)
	len := 0
	for _, r := range s {
		chunk[len] = r
		len++
		if len == chunkSize {
			chunks = append(chunks, string(chunk))
			len = 0
		}
	}
	if len > 0 {
		chunks = append(chunks, string(chunk[:len]))
	}
	return chunks
}

func writeBase64EncodedPart(b []byte, writer *bufio.Writer) {
	sEnc := b64.StdEncoding.EncodeToString(b)
	chunks := chunk(sEnc, 76)
	for _, chunk := range chunks {
		writer.WriteString(chunk)
		writer.WriteString("\n")
		writer.Flush()
	}
}

func CheckBoundary(s string) string {
	b := regexp.MustCompile("boundary")
	if b.MatchString(s) == true {
		runes := []rune(s)
		s2 := "--" + string(runes[27:len(s)-1])
		return string(s2)
	} else {
		return ""
	}
}

func getSubValues(s string) string {

	matcher := regexp.MustCompile(" from| by| for")
	indexes := matcher.FindAllStringIndex(s, -1)

	if len(indexes) < 1 {
		return strings.ReplaceAll(s, ";", ";\r\n        ")
	}

	runes := []rune(s)
	crlf := "\r\n       "
	output := ""
	last := len(indexes) - 1
	for i, idx := range indexes {
		if i == 0 {
			output = string(runes[0:idx[0]])
		} else {
			pi := indexes[i-1]
			output = output + crlf + string(runes[pi[0]:idx[0]])
			if i == last {
				output = output + crlf + string(runes[idx[0]:len(s)])
			}
		}
	}
	return strings.ReplaceAll(output, ";", ";"+crlf)
}
