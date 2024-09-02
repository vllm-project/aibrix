import unittest
from app import app

class FlaskTestCase(unittest.TestCase):
    
    def setUp(self):
        self.client = app.test_client()
    
    def test_metrics(self):
        expected_total = 100
        replica = 3
        response = self.client.get('/metrics')
        self.assertEqual(response.status_code, 200)
        data = response.data.decode()
        print(f"response data: \n<<<<<<<<<<<<<<<<\n{data}\n<<<<<<<<<<<<<<<<")
        # metrics exists
        self.assertIn('vllm:request_success_total', data)
        self.assertIn('vllm:avg_prompt_throughput_toks_per_s', data)
        self.assertIn('vllm:avg_generation_throughput_toks_per_s', data)
        
        # assert metric value
        self.assertIn(f'vllm:request_success_total{{finished_reason="stop",model_name="llama2-70b"}} {expected_total/replica}', data)
        self.assertIn(f'vllm:avg_prompt_throughput_toks_per_s{{model_name="llama2-70b"}} {expected_total/replica}', data)
        self.assertIn(f'vllm:avg_generation_throughput_toks_per_s{{model_name="llama2-70b"}} {expected_total/replica}', data)

if __name__ == '__main__':
    unittest.main()