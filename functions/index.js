const http = require('http');

const handler = async (event) => {
  console.log('Handler called');
  console.log('Event:', event);
  return {
    statusCode: 200,
    body: {
      message: process.env.GREETING || 'Hello from Aether!',
      secret: process.env.MY_SECRET ? '***hidden***' : 'not set',
      nodeEnv: process.env.NODE_ENV || 'development',
      timestamp: new Date().toISOString(),
      input: event,
      allEnv: Object.keys(process.env).filter(k => !k.startsWith('_'))
    }
  };
};

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


