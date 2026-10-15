const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { test } = require('node:test');
const vm = require('node:vm');

// Execute the exact inline function that CloudFormation will publish.
const template = readFileSync(`${__dirname}/cloudfront.yml`, 'utf8');
const code = template.match(/      FunctionCode: \|\n((?:        .*\n|\n)+)/)[1];
const handler = vm.runInNewContext(`${code}\nhandler`);

function request(uri, querystring) {
  return {
    uri,
    method: 'GET',
    headers: { origin: { value: 'https://ima.ge.cx' } },
    querystring,
  };
}

function query(uri, params) {
  return handler({ request: request(uri, params) }).querystring;
}

test('equivalent directory URLs share one key without qualifying stored repository names', () => {
  const expected = 'image=localhost%3A5000%2Fteam%2Fapp&digest=sha256%3Aabc&path=%2F';
  assert.equal(query('/api/dir', {
    digest: { value: 'sha256:abc' },
    hidden: { value: 'false' },
    image: { value: 'localhost%3a5000%2fteam%2fapp%3Av2' },
    ignored: { value: 'tracking' },
  }), expected);
  assert.equal(query('/api/dir', {
    path: { value: '%2f' },
    image: { value: 'localhost:5000/team/app' },
    digest: { value: 'sha256%3Aabc' },
  }), expected);
});

test('different digests, paths and hidden mode remain distinct', () => {
  const params = { image: { value: 'ubuntu:latest' }, digest: { value: 'sha256:abc' } };
  assert.equal(query('/api/file', { ...params, path: { value: 'etc%2Fhosts' }, hidden: { value: 'true' } }),
    'image=ubuntu&digest=sha256%3Aabc&path=etc%2Fhosts&hidden=true');
  assert.equal(query('/api/file', { ...params, path: { value: 'etc/hostname' } }),
    'image=ubuntu&digest=sha256%3Aabc&path=etc%2Fhostname');
  assert.equal(query('/api/file', { ...params, digest: { value: 'sha256:def' }, path: { value: 'etc/hosts' } }),
    'image=ubuntu&digest=sha256%3Adef&path=etc%2Fhosts');
  // A missing file path is not the directory endpoint's default root.
  assert.equal(query('/api/file', params), 'image=ubuntu&digest=sha256%3Aabc&path=');
});

test('image status ignores filesystem parameters and strips only the tag', () => {
  for (const uri of ['/api/info', '/api/metadata']) {
    assert.equal(query(uri, {
      image: { value: 'example/app:v1' }, digest: { value: 'sha256:abc' },
      hidden: { value: 'true' }, path: { value: 'etc/hosts' },
    }), 'image=example%2Fapp&digest=sha256%3Aabc');
  }
});

test('registry lookup keeps tags and ignores unrelated parameters', () => {
  for (const uri of ['/api/lookup', '/api/tags']) {
    assert.equal(query(uri, {
      image: { value: 'example/app:v2' }, digest: { value: 'unused' }, path: { value: '/' },
    }), 'image=example%2Fapp%3Av2');
  }
});

test('duplicate parameters use the first value, including hidden mode', () => {
  assert.equal(query('/api/dir', {
    image: { value: 'ubuntu', multiValue: [{ value: 'ubuntu' }, { value: 'alpine' }] },
    digest: { value: 'sha256:abc' },
    hidden: { value: 'false', multiValue: [{ value: 'false' }, { value: 'true' }] },
  }), 'image=ubuntu&digest=sha256%3Aabc&path=%2F');
});

test('query encoding preserves spaces, literal plus signs and encoded separators', () => {
  assert.equal(query('/api/file', {
    image: { value: 'ubuntu' }, digest: { value: 'sha256:abc' },
    path: { value: 'a+b%2Bc%26d%3Be%25f' }, hidden: { value: '%74rue' },
  }), 'image=ubuntu&digest=sha256%3Aabc&path=a%20b%2Bc%26d%3Be%25f&hidden=true');
});

test('malformed queries are not repaired into cacheable successful requests', () => {
  for (const path of ['bad%ZZ', 'bad;path', '%FF']) {
    const input = request('/api/file', { image: { value: 'ubuntu' }, path: { value: path } });
    assert.deepEqual(handler({ request: structuredClone(input) }), input);
  }
});

test('normalization preserves Origin and does not rewrite unrelated routes', () => {
  const input = request('/api/lookup', { image: { value: 'ubuntu' } });
  const output = handler({ request: structuredClone(input) });
  assert.deepEqual(output.headers, input.headers);
  assert.equal(output.method, input.method);
  assert.equal(output.uri, input.uri);
  const unrelated = request('/other', { extra: { value: 'keep' } });
  assert.deepEqual(handler({ request: structuredClone(unrelated) }), unrelated);
});
