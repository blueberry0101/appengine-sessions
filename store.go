package cascadestore

import (
    "encoding/base32"
    "net/http"

    "fmt"
    "strings"
    "errors"
    "time"

    "google.golang.org/appengine"
    "google.golang.org/appengine/log"
    "google.golang.org/appengine/memcache"
    "google.golang.org/appengine/datastore"

    "github.com/gorilla/securecookie"
    "github.com/gorilla/sessions"
    gcontext "github.com/gorilla/context"
)

var (
    ValueTooBigError = errors.New("SessionStore: the value to store is too big")
)

const (
    RequestBackend = 1 << iota
    MemcacheBackend = 1 << iota
    DatastoreBackend = 1 << iota
)

const (
    // In most cases we won't want to use the "request" backend. Though it's 
    // nice to prevent hitting Memcache or Datastore if the information is 
    // requested multiple times during a single request, it won't be updated by 
    // concurrent requests from the same user/browser. The distributed backends 
    // will receive the updates but the "Request" backend will preempt it with 
    // potentially old information. We'd have to implement a secondary channel, 
    // like the Channel API, to receive fault notifications from other requests 
    // that do an update so that we can know to update the information in the 
    // request. 
    DistributedBackends = MemcacheBackend | DatastoreBackend
    AllBackends = RequestBackend | MemcacheBackend | DatastoreBackend

    // Amount of time for cookies/redis keys to expire.
    DefaultExpireSeconds = 86400 * 30
    MaxValueLength = 4096
    DefaultMaxAgeSeconds = 60 * 20
    DefaultKeyPrefix = "session."
)

// For datastore.
type sessionKind struct {
    Value []byte
    ExpiresAt time.Time
}

type requestItem struct {
    Value []byte
    ExpiresAt time.Time
}

type CascadeStore struct {
    backendTypes  int
    maxLength     int
    keyPrefix     string
    serializer    SessionSerializer

    Codecs        []securecookie.Codec
    Options       *sessions.Options // default configuration
    DefaultMaxAge int               // default Redis TTL for a MaxAge == 0 session
}

func NewCascadeStore(backendTypes int, keyPairs ...[]byte) *CascadeStore {
    return &CascadeStore{
        backendTypes: backendTypes,
        maxLength: MaxValueLength,
        keyPrefix: DefaultKeyPrefix,
        serializer: GobSerializer{},

        Codecs: securecookie.CodecsFromPairs(keyPairs...),
        Options: &sessions.Options{
            Path: "/",
            MaxAge: DefaultExpireSeconds,
        },
        DefaultMaxAge: DefaultMaxAgeSeconds, // 20 minutes seems like a reasonable default
    }
}

// SetMaxLength sets RediStore.maxLength if the `l` argument is greater or equal 0
// maxLength restricts the maximum length of new sessions to l.
// If l is 0 there is no limit to the size of a session, use with caution.
// The default for a new RediStore is 4096. Redis allows for max.
// value sizes of up to 512MB (http://redis.io/topics/data-types)
// Default: 4096,
func (cs *CascadeStore) SetMaxLength(l int) {
    if l >= 0 {
        cs.maxLength = l
    }
}

// SetKeyPrefix set the prefix
func (cs *CascadeStore) SetKeyPrefix(p string) {
    cs.keyPrefix = p
}

// SetSerializer sets the serializer
func (cs *CascadeStore) SetSerializer(ss SessionSerializer) {
    cs.serializer = ss
}

// SetMaxAge restricts the maximum age, in seconds, of the session record
// both in database and a browser. This is to change session storage configuration.
// If you want just to remove session use your session `s` object and change it's
// `Options.MaxAge` to -1, as specified in
//    http://godoc.org/github.com/gorilla/sessions#Options
//
// Default is the one provided by this package value - `DefaultExpireSeconds`.
// Set it to 0 for no restriction.
// Because we use `MaxAge` also in SecureCookie crypting algorithm you should
// use this function to change `MaxAge` value.
func (cs *CascadeStore) SetMaxAge(v int) {
    var c *securecookie.SecureCookie
    var ok bool
    cs.Options.MaxAge = v
    for i := range cs.Codecs {
        if c, ok = cs.Codecs[i].(*securecookie.SecureCookie); ok {
            c.MaxAge(v)
        } else {
            panic(fmt.Errorf("Can't change MaxAge on codec %v\n", cs.Codecs[i]))
        }
    }
}

// Get returns a session for the given name after adding it to the registry.
//
// See gorilla/sessions FilesystemStore.Get().
func (cs *CascadeStore) Get(r *http.Request, name string) (*sessions.Session, error) {
    return sessions.GetRegistry(r).Get(cs, name)
}

// New returns a session for the given name without adding it to the registry.
//
// See gorilla/sessions FilesystemStore.New().
func (cs *CascadeStore) New(r *http.Request, name string) (*sessions.Session, error) {
    var err error
    session := sessions.NewSession(cs, name)

    // make a copy
    options := *cs.Options
    session.Options = &options

    session.IsNew = true
    if c, errCookie := r.Cookie(name); errCookie == nil {
        err = securecookie.DecodeMulti(name, c.Value, &session.ID, cs.Codecs...)
        if err == nil {
            ok, err := cs.load(r, session)
            session.IsNew = !(err == nil && ok) // not new if no error and data available
        }
    }
    return session, err
}

// Save adds a single session to the response.
func (cs *CascadeStore) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
    // Marked for deletion.
    if session.Options.MaxAge < 0 {
        if err := cs.delete(r, session); err != nil {
            return err
        }

        http.SetCookie(w, sessions.NewCookie(session.Name(), "", session.Options))
    } else {
        // Build an alphanumeric key for the redis store.
        if session.ID == "" {
            session.ID = strings.TrimRight(base32.StdEncoding.EncodeToString(securecookie.GenerateRandomKey(32)), "=")
        }

        if err := cs.save(r, session); err != nil {
            return err
        }

        encoded, err := securecookie.EncodeMulti(session.Name(), session.ID, cs.Codecs...)
        if err != nil {
            return err
        }

        http.SetCookie(w, sessions.NewCookie(session.Name(), encoded, session.Options))
    }
    return nil
}

// save stores the session in redis.
func (cs *CascadeStore) save(r *http.Request, session *sessions.Session) (err error) {
    ctx := appengine.NewContext(r)

    defer func() {
        if r := recover(); r != nil {
            err := r.(error)
            log.Errorf(ctx, "Could not save session: %s", err)
        }
    }()

    log.Debugf(ctx, "Saving session: [%s]", session.ID)

    key := cs.keyPrefix + session.ID

    var serialized []byte
    if serialized, err = cs.serializer.Serialize(session); err != nil {
        panic(err)
    } else if cs.maxLength != 0 && len(serialized) > cs.maxLength {
        panic(ValueTooBigError)
    }

    age := session.Options.MaxAge
    if age == 0 {
        age = cs.DefaultMaxAge
    }

    expires := time.Second * time.Duration(age)
    expiresAt := time.Now().Add(expires)

    if (cs.backendTypes & RequestBackend) > 0 {
        log.Debugf(ctx, "Writing session to Request: [%s]", key)

        item := &requestItem{
            Value: serialized,
            ExpiresAt: expiresAt,
        }

        gcontext.Set(r, key, item)
    }

    if (cs.backendTypes & MemcacheBackend) > 0 {
        log.Debugf(ctx, "Writing session to Memcache: [%s]", key)

        item := &memcache.Item{
            Key: key,
            Value: serialized,
            Expiration: expires,
        }

        if err := memcache.Set(ctx, item); err != nil {
            panic(err)
        }
    }

    if (cs.backendTypes & DatastoreBackend) > 0 {
        log.Debugf(ctx, "Writing session to Datastore: [%s]", key)

        s := &sessionKind{
            Value: serialized,
            ExpiresAt: expiresAt,
        }

        k := datastore.NewKey(ctx, "Session", key, 0, nil)
        if _, err := datastore.Put(ctx, k, s); err != nil {
            panic(err)
        }
    }

    return nil
}

// load reads the session from redis.
// returns true if there is a sessoin data in DB
func (cs *CascadeStore) load(r *http.Request, session *sessions.Session) (success bool, err error) {
    ctx := appengine.NewContext(r)

    defer func() {
        if r := recover(); r != nil {
            err := r.(error)
            log.Errorf(ctx, "Could not load session: %s", err)
        }
    }()

    success = false

    log.Debugf(ctx, "Loading session: [%s]", session.ID)

    key := cs.keyPrefix + session.ID
    var value []byte
    now := time.Now()

    if value == nil && (cs.backendTypes & RequestBackend) > 0 {
        // Try request.

        itemRaw := gcontext.Get(r, key)
        if itemRaw != nil {
            item := itemRaw.(requestItem)
            if now.Before(item.ExpiresAt) {
                value = item.Value
                log.Debugf(ctx, "Found session in request: [%s]", key)
            } else {
                gcontext.Delete(r, key)
            }
        }
    }

    if value == nil && (cs.backendTypes & MemcacheBackend) > 0 {
        // Try memcache.

        var item *memcache.Item
        if item, err = memcache.Get(ctx, key); err != nil {
            if err == memcache.ErrCacheMiss {
                log.Debugf(ctx, "Could not find session in Memcache: [%s]", key)
            } else {
                panic(err)
            }
        } else if err == nil {
            value = item.Value
            log.Debugf(ctx, "Found session in Memcache: [%s]", key)
        }
    }

    if value == nil && (cs.backendTypes & DatastoreBackend) > 0 {
        // Try datastore.

        k := datastore.NewKey(ctx, "Session", key, 0, nil)
        s := &sessionKind{}
        if err := datastore.Get(ctx, k, s); err != nil {
            if err == datastore.ErrNoSuchEntity {
                log.Debugf(ctx, "Could not find session in Datastore: [%s]", key)
            } else {
                panic(err)
            }
        } else if err == nil {
            if now.Before(s.ExpiresAt) {
                value = s.Value
                log.Debugf(ctx, "Found session in Datastore: [%s]", key)
            } else if err := cs.delete(r, session); err != nil {
                panic(err)
            }
        }
    }

    if value != nil {
        if err := cs.serializer.Deserialize(value, session); err != nil {
            panic(err)
        }

        success = true
    }

    return success, nil
}

// delete removes keys from redis if MaxAge<0
func (cs *CascadeStore) delete(r *http.Request, session *sessions.Session) (err error) {
    ctx := appengine.NewContext(r)

    defer func() {
        if r := recover(); r != nil {
            err := r.(error)
            log.Errorf(ctx, "Could not delete session: %s", err)
        }
    }()

    log.Debugf(ctx, "Deleting session: [%s]", session.ID)

    key := cs.keyPrefix + session.ID

    if (cs.backendTypes & RequestBackend) > 0 {
        log.Debugf(ctx, "Removing session from Request: [%s]", key)
        gcontext.Delete(r, key)
    }

    if (cs.backendTypes & MemcacheBackend) > 0 {
        log.Debugf(ctx, "Removing session from Memcache: [%s]", key)

        if err := memcache.Delete(ctx, key); err != nil {
            if err == memcache.ErrCacheMiss {
                log.Warningf(ctx, "Tried and failed to remove old session from Memcache: [%s]", key)
            } else {
                panic(err)
            }
        }
    }

    if (cs.backendTypes & DatastoreBackend) > 0 {
        log.Debugf(ctx, "Removing session from Datastore: [%s]", key)

        k := datastore.NewKey(ctx, "Session", key, 0, nil)
        if err := datastore.Delete(ctx, k); err != nil {
            log.Warningf(ctx, "Tried and failed to remove old session from Datastore: [%s]", key)
        }
    }

    return nil
}
