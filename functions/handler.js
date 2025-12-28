const http = require('http');

// Your function logic
const handler = async (event) => {

  
  return {
    statusCode: 200,
    body: {
      message: 'Hello function! This is a test!',
      timestamp: new Date().toISOString(),
      input: event
    }
  };
};

// HTTP server wrapper
const server = http.createServer(async (req, res) => {
  let body = '';
  
  req.on('data', chunk => { body += chunk; });
  
  req.on('end', async () => {
    try {
      const event = body ? JSON.parse(body) : { method: req.method, url: req.url };
      const result = await handler(event);
      res.writeHead(result.statusCode, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(result.body));
    } catch (err) {
      res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: err.message }));
    }
  });
});

server.listen(3000, '0.0.0.0', () => {
  console.log('Function ready on port 3000');
});


