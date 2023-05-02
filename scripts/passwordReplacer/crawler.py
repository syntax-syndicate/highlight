import boto3
import os
import sys
from multiprocessing import Pool

bucket = 'highlight-session-data'

s3 = boto3.client('s3', 
    aws_access_key_id=os.environ.get('AWS_ACCESS_KEY_ID'),
    aws_secret_access_key=os.environ.get('AWS_SECRET_ACCESS_KEY'),
    region_name="us-east-2",
)
lambdaClient = boto3.client('lambda', 
    aws_access_key_id=os.environ.get('AWS_ACCESS_KEY_ID'),
    aws_secret_access_key=os.environ.get('AWS_SECRET_ACCESS_KEY'),
    region_name="us-east-2",
)

def f(key):
    lambdaClient.invoke(
        FunctionName='passwordReplacer',
        InvocationType='Event',
        Payload=f'''{{
            "Records": [
                {{
                    "s3": {{
                        "bucket": {{
                            "name": "{bucket}"
                        }},
                        "object": {{
                            "key": "{key}"
                        }}
                    }}
                }}
            ]
        }}'''
    )

if __name__ == "__main__":

    paginator = s3.get_paginator('list_objects_v2')

    cfg = {}
    if len(sys.argv) >= 3:
        cfg = {'PaginationConfig': {'StartingToken': sys.argv[2]}}

    pages = paginator.paginate(Bucket=bucket, Prefix=sys.argv[1], **cfg)
    with Pool(250) as p:
        for page in pages:
            print(page['NextContinuationToken'])
            items = []
            for obj in page['Contents']:
                key = obj['Key']
                if 'session-contents-compressed-' in key:
                    items.append(key)
            p.map(f, items)