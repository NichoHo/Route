import http from 'k6/http';
import { check, sleep } from 'k6';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

export const options = {
    vus: 50,
    duration: '30s',
};

export default function () {
    const url = 'http://host.docker.internal:8080/orders';
    
    // Generate a random pickup location in Jakarta
    const lat = -6.23 + Math.random() * (-6.17 - -6.23);
    const lng = 106.80 + Math.random() * (106.85 - 106.80);
    
    const payload = JSON.stringify({
        pickup_lat: lat,
        pickup_lng: lng,
        dropoff_lat: Math.min(lat + 0.01, -6.17),
        dropoff_lng: Math.min(lng + 0.01, 106.85),
        priority: 1
    });

    const params = {
        headers: {
            'Content-Type': 'application/json',
            'Idempotency-Key': uuidv4(),
            'Authorization': `Bearer ${__ENV.ROUTE_API_TOKEN}`,
        },
    };

    const res = http.post(url, payload, params);
    
    check(res, {
        'status is 201': (r) => r.status === 201,
    });
    
    sleep(1);
}
