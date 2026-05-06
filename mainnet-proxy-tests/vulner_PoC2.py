import os
from gonka_openai import GonkaOpenAI
from gonka_openai import gonka_http_client
from gonka_openai.utils import Endpoint
import threading
import random
import datetime

gonka_private_key=os.environ.get('GONKA_PRIVATE_KEY')

private_keys = [
  gonka_private_key,
]

endpoints = [
    Endpoint(url="http://node1.gonka.ai:8000/v1", address="gonka1y2a9p56kv044327uycmqdexl7zs82fs5ryv5le"),
	Endpoint(url="http://node2.gonka.ai:8000/v1", address="gonka1dkl4mah5erqggvhqkpc8j3qs5tyuetgdy552cp")
]

def custom_strategy(endpoints):
    """Always select the first endpoint."""
    array=random.randint(0, len(endpoints) - 1)
    return endpoints[array]

def _run_client(key):
    """Execute a single request using the provided private key."""
    try:
        client = GonkaOpenAI(
            gonka_private_key=key,
            endpoints=endpoints,
            endpoint_selection_strategy=custom_strategy,
            timeout=600
        )

        simplified_sql_grammar = """
            ?start: select_statement

            ?select_statement: "SELECT " column_list " FROM " table_name

            ?column_list: column_name ("," column_name)*

            ?table_name: identifier

            ?column_name: identifier

            ?identifier: /[a-zA-Z_][a-zA-Z0-9_]*/
        """

        response = client.chat.completions.create(
            model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
            messages=[
                {
                    "role": "user",
                    "content": "Hello World"
                }
            ],
            max_tokens=100,
            extra_body={"guided_grammar": simplified_sql_grammar},

        )
        print(response.choices[0].message.content)
    except Exception as e:
        print(f"Error!: {e}")
_run_client(gonka_private_key)
print("END")