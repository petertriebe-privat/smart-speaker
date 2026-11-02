"""Check whether the Google refresh token in the private secrets still works (prints no secrets).

Usage: py -3 tools/check_google.py
"""
import json
import urllib.error
import urllib.parse
import urllib.request

from make_secrets import OUT as SECRETS


def main():
    s = json.load(open(SECRETS, encoding='utf-8'))
    form = urllib.parse.urlencode({'client_id': s['GOOGLE_CLIENT_ID'], 'client_secret': s['GOOGLE_CLIENT_SECRET'],
                                   'refresh_token': s['GOOGLE_REFRESH_TOKEN'], 'grant_type': 'refresh_token'}).encode()
    try:
        reply = json.load(urllib.request.urlopen('https://oauth2.googleapis.com/token', form, timeout=20))
        print('token ok, expires in %s s, scopes: %s' % (reply.get('expires_in'), reply.get('scope', '?')))
    except urllib.error.HTTPError as e:
        body = json.loads(e.read() or b'{}')
        print('refused: HTTP %d %s' % (e.code, body.get('error')))


if __name__ == '__main__':
    main()
