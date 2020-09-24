package main

import (
  "context"
  "encoding/json"
  "errors"
  "fmt"
  "io/ioutil"
  "log"
  "net/http"
  "os"
  "strconv"
  "strings"
  "time"

  // DB.
  "cloud.google.com/go/datastore"

  secretmanager "cloud.google.com/go/secretmanager/apiv1"
  secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
)

// *****************
// Request handling.
// *****************

// A lightweight Date.
// Unfortunately the time module doesn't define this
// but returns the triple instead.
type Date struct {
  Year int
  Month int
  Day int
}

func (d Date) Is(other Date) bool {
  return d.Year == other.Year && d.Month == other.Month && d.Day == other.Day
}

func (d Date) After(other Date) bool {
  return d.Year > other.Year || d.Month > other.Month || d.Day > other.Day
}

func (d Date) toString() string {
  // We use 0 padded RFC3339 representation of the date as our primary
  // key. This works as the lexicographic order follows the calendar's.
  // We pad the year with a zero to be safe. Also it matches the format
  // from Long Now Foundation, which is cool!
  return fmt.Sprintf("%05d-%02d-%02d", d.Year, d.Month, d.Day)
}

func parseDate(date string) (*Date, error) {
  if len(date) != 11 {
    return nil, errors.New("Wrong size for date=" + date)
  }

  year, err := strconv.Atoi(date[0:5])
  if err != nil {
    return nil, err
  }
  month, err := strconv.Atoi(date[6:8])
  if err != nil {
    return nil, err
  }
  if month > 12 {
    return nil, errors.New("Invalid month for date=" + date)
  }
  day, err := strconv.Atoi(date[9:11])
  if err != nil {
    return nil, err
  }
  if day > 31 {
    return nil, errors.New("Invalid day for date=" + date)
  }

  return &Date{
    year,
    month,
    day,
  }, nil
}

func convertToDate(date time.Time) Date {
  return Date{
    date.Year(),
    int(date.Month()),
    date.Day(),
  }
}

func logRequest(req *http.Request) {
  log.Printf("Received request for %s", req.URL.String())
}

func mainPageHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  if req.URL.Path != "/" {
      http.NotFound(w, req)
      return
  }

  // TODO: Check for login credentials.

  http.ServeFile(w, req, "index.html")
}

func newestRunHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  newestRun, err := GetNewestRun()
  if err != nil {
    log.Printf("[ERROR] Failed getting the runs, err=%v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  if newestRun == nil {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Write([]byte("null"))
    return
  }

  payload, err := json.Marshal(newestRun)
  if err != nil {
    log.Printf("[ERROR] Failed marshalling newestRun for response=%v, err=%v", newestRun, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }
  w.Header().Set("Content-Type", "application/json; charset=utf-8")
  w.Write([]byte(payload))
}

func scheduleRunHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  date := getNextScheduledMessageTime()
  run := Run{
    // TODO: We can probably get away with some string conversion.
    // I couldn't make it work though...
    date.toString(),
    []Event{},
    nil}
  err := UpsertRun(run)
  if err != nil {
    log.Printf("[ERROR] Failed to upsert new run, err=%v", err)
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Write([]byte("false"))
    return
  }

  w.Header().Set("Content-Type", "application/json; charset=utf-8")
  w.Write([]byte("true"))
}

func cronHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  // Check header: X-Appengine-Cron: true

  newestRun, err := GetNewestRun()
  if err != nil {
    log.Printf("[ERROR] Failed to get newest run=%v, err=%v", newestRun, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  runDate, err := parseDate(newestRun.Date)
  if err != nil {
    log.Printf("[ERROR] Failed to parse the date of the newest run=%v, err=%v", newestRun, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  today := convertToDate(time.Now())
  if !runDate.Is(today) {
    log.Printf("Nothing to do today=%s, runDate=%s", today.toString(), runDate.toString())
    w.Write([]byte("Nothing to do"))
    return
  }

  log.Printf("Running today=%s", today.toString())

  err = postBlockMessageToChannel(string(messagePayload))
  if err != nil {
    log.Printf("Couldn't post: %v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  w.Write([]byte("Message sent"))
  // TODO: Add the next event!
}

func testMessageHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  err := postBlockMessageToChannel(string(testPayload))
  if err != nil {
    log.Printf("Couldn't post: %v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  w.Write([]byte("Message sent"))
}

// ***************
// Time management
// ***************

func getSecondThursdayForYearAndMonth(year int, month time.Month, pst *time.Location) time.Time {
  // Start at the first of the month and walk the Calendar until we find a Thursday.
  firstThursday := time.Date(year, month, /*day=*/1, /*hour=*/9, /*min=*/0, /*sec=*/0, /*nsec=*/0, pst)
  for i := 1; i <= 7; i++ {
    if firstThursday.Weekday() == time.Thursday {
      break
    }

    firstThursday = firstThursday.AddDate(/*years=*/0, /*months=*/0, /*days=*/1)
  }

  return firstThursday.AddDate(/*years=*/0, /*months=*/0, /*days=*/7)
}

func getNextScheduledMessageTime() Date {
  today := time.Now()
  location := today.UTC().Location()
  // Check this month for the next date.
  // If it is passed, we look for the scheduled time next month.
  secondThursdayOfThisMonth := getSecondThursdayForYearAndMonth(today.Year(), today.Month(), location)
  if (secondThursdayOfThisMonth.After(today)) {
    return convertToDate(secondThursdayOfThisMonth)
  }

  // This call correctly handles December as Date wraps the month into the new year.
  return convertToDate(getSecondThursdayForYearAndMonth(today.Year(), today.Month() + 1, location))
}



// **************
// DB management.
// **************

// TODO: const PROJECT_ID string = "pricebook"
const RUN_TABLE string = "Runs"

type Event struct {
  User string `json:"user", datastore:",noindex"`
  TimestampSec string `json:"timestamp_sec", datastore:",noindex"`
}

type Run struct {
  // This is the hexadecimal representation of the Key.ID.
  // It is not stored but is sometimes returned to our API.
  // It is mandatory for updating and deleting.
  //ID string `json:"id", datastore:"-"`

  // TODO: Should I store it as a time.Time?
  // Currently date serialized using RFC3339.
  Date string `json:"date"`
  Postponements []Event `json:"postponements",datastore:",noindex"`
  Cancellation *Event `json:"cancellation",datastore:",noindex"`
}

func GetNewestRun() (*Run, error) {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return nil, err
  }

  var newestRun []Run
  q := datastore.NewQuery(RUN_TABLE).Order("-Date").Limit(1)
  if _, err := client.GetAll(ctx, q, &newestRun); err != nil {
    return nil, err
  }

  if len(newestRun) < 1 {
    return nil, nil
  }

  return &newestRun[0], nil
}

func UpsertRun(run Run) error {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return err
  }
  k := datastore.NameKey(RUN_TABLE, run.Date, nil)
  key, err := client.Put(ctx, k, &run)
  log.Printf("Key from put=%v", key)
  return err
}

// TODO: Remove this and use the DB to store the token.
// ***************
// Secret handling
// ***************

func getBotToken() (string, error) {
  ctx := context.Background()
  client, err := secretmanager.NewClient(ctx)
  if err != nil {
          return "", err
  }

  req := &secretmanagerpb.AccessSecretVersionRequest{
          Name: os.Getenv("BOT_TOKEN_SECRET"),
  }
  result, err := client.AccessSecretVersion(ctx, req)
  if err != nil {
          return "", err
  }

  // Some editors leave some trailing \n in the secret so trim them out.
  return strings.TrimSpace(string(result.Payload.Data)), nil
}

// ***************
// Slack messaging
// ***************

const messagePayload string = `[
  {
    "type": "rich_text_section",
    "elements": [
      {
        "type": "broadcast",
        "range": "channel"
      }
    ]
  },
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "*HHH is in one week*"
    }
  },
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "Add your availability with an emoji: :no: if you're unavailable. Anything else for yes :party-parrot:"
    }
  },
  {
    "type": "divider",
  },
  {
    "type": "actions",
    "elements": [
      {
        "type": "button",
        "text": {
          "type": "plain_text",
          "text": "Skip this one :sadpanda:",
          "emoji": true
        },
        "value": "skip"
      }
    ]
  },
  {
    "type": "actions",
    "elements": [
      {
        "type": "button",
        "text": {
          "type": "plain_text",
          "text": "Postpone by one week",
          "emoji": true
        },
        "value": "skip"
      }
    ]
  }
]`

const testPayload string = `[
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "Test message :deal-with-it-parrot:"
    }
  },
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "Brought to you by :babyyoda:"
    }
  }
]`

func postBlockMessageToChannel(payload string) error {
  botToken, err := getBotToken()
  if err != nil {
    return err
  }

  fullPayload := fmt.Sprintf("{\"channel\": \"%s\",\"blocks\": %s }", os.Getenv("CHANNEL_ID"), payload)
  log.Printf("Payload to be send: %s", fullPayload)
  bodyReader := strings.NewReader(fullPayload)
  req, err := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bodyReader)
  req.Header.Add("Content-Type", "application/json; charset=utf-8")
  req.Header.Add("Authorization", "Bearer " + botToken)

  defaultClient := &http.Client{}
  resp, err := defaultClient.Do(req)
  if err != nil {
	return err
  }
  defer resp.Body.Close()
  body, err := ioutil.ReadAll(resp.Body)
  if err != nil {
	return err
  }
  log.Printf("Response: %s", string(body))
  return nil
}


// ****
// main
// ****

func main() {
  http.HandleFunc("/", mainPageHandler)
  http.HandleFunc("/newestRun", newestRunHandler)
  http.HandleFunc("/scheduleRun", scheduleRunHandler)
  http.HandleFunc("/cron", cronHandler)
  http.HandleFunc("/testMessage", testMessageHandler)

  port := os.Getenv("PORT")
  if port == "" {
    port = "8080"
  }

  log.Printf("Listening on port=%s", port)
  if err := http.ListenAndServe(":" + port, nil); err != nil {
    log.Fatal(err)
  }
}
